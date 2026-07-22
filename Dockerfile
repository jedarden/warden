# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /warden ./cmd/warden

FROM gcr.io/distroless/static:nonroot
COPY --from=build /warden /warden
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/warden"]
