# Two stages: build static binaries, then copy them into an image with
# nothing else in it. distroless/static has CA certificates, for calling
# providers over HTTPS, and runs as a non-root user.

FROM golang:1.27.1-alpine AS build
WORKDIR /src
# Download modules first, so code changes don't invalidate that layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/tollgate ./cmd/mockupstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tollgate /out/mockupstream /usr/local/bin/
# config.yaml is for docker compose; demo.yaml for the hosted demo.
COPY deploy/config.yaml deploy/demo.yaml /etc/tollgate/
# Commands run in the config directory, so their default config path,
# config.yaml, finds the file: `serve`, and `keys create -models`, which
# checks aliases against it.
WORKDIR /etc/tollgate
# 8080 is the API; 6060 the admin listener (metrics and pprof), which
# docker-compose.yml keeps on the internal network.
EXPOSE 8080 6060
# No ENTRYPOINT: a command given to the container is the whole command, so
# `tollgate keys ...`, `mockupstream ...`, and a host's start-command
# override such as Render's all work the same way.
CMD ["tollgate", "serve"]
