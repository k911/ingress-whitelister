ARG GOLANG_TAG="1.11-alpine"
FROM golang:${GOLANG_TAG} as Go

RUN apk add --no-cache git gcc musl-dev

WORKDIR /usr/src/app
ENTRYPOINT ["go"]

COPY go.mod go.sum ./

RUN go mod download
RUN go mod verify

COPY main.go ./
RUN CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -a -tags netgo -ldflags "-w" -o "ingress-whitelister" "main.go"

FROM scratch as Controller
WORKDIR /usr/src/app

COPY --from=Go /usr/src/app/ingress-whitelister ./ingress-whitelister

ENTRYPOINT ["./ingress-whitelister"]