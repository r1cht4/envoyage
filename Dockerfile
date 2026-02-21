FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /envoyage-cp ./cmd/controlplane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /envoyage-cp /usr/local/bin/envoyage-cp
ENTRYPOINT ["envoyage-cp"]
