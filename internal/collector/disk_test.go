package collector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	stderrors "errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/tekikaito/kshows/internal/model"
)

const summaryJSON = `{"node":{"nodeName":"n1","fs":{"capacityBytes":100,"usedBytes":40,"availableBytes":60}}}`

// kubeletServer is a TLS server standing in for a kubelet. It answers
// /stats/summary with status, and only to the bearer token "tok".
func kubeletServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/summary" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(summaryJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// nodeFor returns a Node whose InternalIP and kubelet port point at srv.
func nodeFor(t *testing.T, srv *httptest.Server) *corev1.Node {
	t.Helper()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: host}}
	n.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(p)
	return n
}

// unrelatedCA returns a fresh self-signed CA certificate. Every httptest
// server shares one built-in certificate, so a second server is not one.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func serverCA(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestKubeletDirect(t *testing.T) {
	t.Run("verified TLS and the token reach the summary", func(t *testing.T) {
		srv := kubeletServer(t, http.StatusOK)
		client, err := newKubeletClient(&rest.Config{BearerToken: "tok", TLSClientConfig: rest.TLSClientConfig{CAData: serverCA(srv)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		c := &Collector{kubeletHTTP: client}
		got, err := c.fetchKubeletDisk(context.Background(), nodeFor(t, srv))
		if err != nil {
			t.Fatal(err)
		}
		if want := (model.Disk{CapacityBytes: 100, UsedBytes: 40, AvailableBytes: 60, Live: true}); got != want {
			t.Errorf("disk = %+v, want %+v", got, want)
		}
	})

	t.Run("an untrusted kubelet certificate is refused", func(t *testing.T) {
		srv := kubeletServer(t, http.StatusOK)
		client, _ := newKubeletClient(&rest.Config{BearerToken: "tok", TLSClientConfig: rest.TLSClientConfig{CAData: unrelatedCA(t)}}, false)
		c := &Collector{kubeletHTTP: client}
		if _, err := c.fetchKubeletDisk(context.Background(), nodeFor(t, srv)); err == nil {
			t.Fatal("fetch succeeded against an untrusted certificate")
		}
	})

	t.Run("insecure TLS accepts a self-signed kubelet", func(t *testing.T) {
		srv := kubeletServer(t, http.StatusOK)
		client, _ := newKubeletClient(&rest.Config{BearerToken: "tok", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ignored")}}, true)
		c := &Collector{kubeletHTTP: client}
		if _, err := c.fetchKubeletDisk(context.Background(), nodeFor(t, srv)); err != nil {
			t.Fatalf("fetch with insecure TLS: %v", err)
		}
	})

	statuses := []struct {
		name          string
		token         string
		status        int
		wantForbidden bool
	}{
		{"403 is forbidden", "tok", http.StatusForbidden, true},
		{"401 is forbidden", "wrong", http.StatusOK, true},
		{"500 is transient", "tok", http.StatusInternalServerError, false},
	}
	for _, tt := range statuses {
		t.Run(tt.name, func(t *testing.T) {
			srv := kubeletServer(t, tt.status)
			client, _ := newKubeletClient(&rest.Config{BearerToken: tt.token, TLSClientConfig: rest.TLSClientConfig{CAData: serverCA(srv)}}, false)
			c := &Collector{kubeletHTTP: client}
			_, err := c.fetchKubeletDisk(context.Background(), nodeFor(t, srv))
			if err == nil {
				t.Fatal("want an error")
			}
			if got := apierrors.IsForbidden(err); got != tt.wantForbidden {
				t.Errorf("IsForbidden(%v) = %v, want %v", err, got, tt.wantForbidden)
			}
		})
	}
}

func TestKubeletAddressAndPort(t *testing.T) {
	n := &corev1.Node{}
	n.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: "node-1"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
		{Type: corev1.NodeInternalIP, Address: "10.0.0.7"},
	}
	if got := kubeletAddress(n); got != "10.0.0.7" {
		t.Errorf("address = %q, want the InternalIP", got)
	}
	n.Status.Addresses = n.Status.Addresses[:2]
	if got := kubeletAddress(n); got != "203.0.113.7" {
		t.Errorf("address = %q, want the ExternalIP without an InternalIP", got)
	}
	if got := kubeletPort(n); got != defaultKubeletPort {
		t.Errorf("port = %d, want the default %d when the Node reports none", got, defaultKubeletPort)
	}
}

func TestDiskRouteSelection(t *testing.T) {
	disk := model.Disk{CapacityBytes: 1, Live: true}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "n1", stderrors.New("denied"))
	unreachable := stderrors.New("dial tcp: connection refused")
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}}

	// collectorWith stubs both routes; each answers with *err (nil = success)
	// and counts its calls.
	collectorWith := func(mode string, kubeletErr, proxyErr *error) (*Collector, *int, *int) {
		var kCalls, pCalls int
		c := &Collector{diskMode: mode, logf: t.Logf}
		c.kubeletDisk = func(context.Context, *corev1.Node) (model.Disk, error) {
			kCalls++
			return disk, *kubeletErr
		}
		c.proxyDisk = func(context.Context, *corev1.Node) (model.Disk, error) {
			pCalls++
			return disk, *proxyErr
		}
		return c, &kCalls, &pCalls
	}
	ctx := context.Background()

	t.Run("auto prefers the kubelet and sticks with it", func(t *testing.T) {
		var kErr, pErr error
		c, k, p := collectorWith(NodeDiskAuto, &kErr, &pErr)
		for range 3 {
			if _, err := c.fetchDisk(ctx, nodes); err != nil {
				t.Fatal(err)
			}
		}
		if *k != 3 || *p != 0 || c.diskRoute != NodeDiskKubelet {
			t.Errorf("kubelet calls %d, proxy calls %d, route %q; want 3, 0, kubelet", *k, *p, c.diskRoute)
		}
	})

	t.Run("auto falls back to the proxy and sticks with it", func(t *testing.T) {
		kErr, pErr := unreachable, error(nil)
		c, k, p := collectorWith(NodeDiskAuto, &kErr, &pErr)
		for range 3 {
			if _, err := c.fetchDisk(ctx, nodes); err != nil {
				t.Fatal(err)
			}
		}
		if *k != 1 || *p != 3 || c.diskRoute != NodeDiskProxy {
			t.Errorf("kubelet calls %d, proxy calls %d, route %q; want 1, 3, proxy", *k, *p, c.diskRoute)
		}
	})

	t.Run("auto reports forbidden when a permission is part of the failure", func(t *testing.T) {
		for _, tt := range []struct {
			name           string
			kubelet, proxy error
			wantForbidden  bool
		}{
			{"both forbidden", forbidden, forbidden, true},
			{"kubelet unreachable, proxy forbidden", unreachable, forbidden, true},
			{"kubelet forbidden, proxy unreachable", forbidden, unreachable, true},
			{"both unreachable", unreachable, unreachable, false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				kErr, pErr := tt.kubelet, tt.proxy
				c, _, _ := collectorWith(NodeDiskAuto, &kErr, &pErr)
				_, err := c.fetchDisk(ctx, nodes)
				if err == nil {
					t.Fatal("want an error")
				}
				if got := apierrors.IsForbidden(err); got != tt.wantForbidden {
					t.Errorf("IsForbidden(%v) = %v, want %v", err, got, tt.wantForbidden)
				}
				if c.diskRoute != "" {
					t.Errorf("route = %q, want none settled", c.diskRoute)
				}
			})
		}
	})

	t.Run("a revoked permission makes auto probe again", func(t *testing.T) {
		var kErr, pErr error
		c, k, p := collectorWith(NodeDiskAuto, &kErr, &pErr)
		_, _ = c.fetchDisk(ctx, nodes) // settles on the kubelet
		kErr = forbidden
		if _, err := c.fetchDisk(ctx, nodes); !apierrors.IsForbidden(err) {
			t.Fatalf("err = %v, want forbidden", err)
		}
		if c.diskRoute != "" {
			t.Fatalf("route = %q after a 403, want it cleared", c.diskRoute)
		}
		if _, err := c.fetchDisk(ctx, nodes); err != nil {
			t.Fatal(err)
		}
		if c.diskRoute != NodeDiskProxy || *k != 3 || *p != 1 {
			t.Errorf("route %q, kubelet calls %d, proxy calls %d; want proxy, 3, 1", c.diskRoute, *k, *p)
		}
	})

	t.Run("an explicit route never touches the other", func(t *testing.T) {
		var kErr, pErr error = forbidden, forbidden
		c, k, p := collectorWith(NodeDiskKubelet, &kErr, &pErr)
		_, _ = c.fetchDisk(ctx, nodes)
		if *k != 1 || *p != 0 {
			t.Errorf("kubelet mode: kubelet calls %d, proxy calls %d; want 1, 0", *k, *p)
		}
		c, k, p = collectorWith(NodeDiskProxy, &kErr, &pErr)
		_, _ = c.fetchDisk(ctx, nodes)
		if *k != 0 || *p != 1 {
			t.Errorf("proxy mode: kubelet calls %d, proxy calls %d; want 0, 1", *k, *p)
		}
	})
}
