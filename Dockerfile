FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kshows ./cmd/kshows

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kshows /kshows
EXPOSE 8080
ENTRYPOINT ["/kshows"]
