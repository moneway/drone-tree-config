FROM golang:alpine AS builder

RUN true \
  && apk add -U --no-cache ca-certificates git binutils

ADD . /go/src/github.com/moneway/drone-tree-config
WORKDIR /go/src/github.com/moneway/drone-tree-config

ENV CGO_ENABLED=0

RUN true \
  && go get -u github.com/golang/dep/cmd/dep \
  && dep ensure -v \
  && go build -o drone-tree-config github.com/moneway/drone-tree-config/cmd/drone-tree-config \
  && strip drone-tree-config

# ---

FROM alpine

RUN true \
  && apk add -U --no-cache ca-certificates
COPY --from=builder /go/src/github.com/moneway/drone-tree-config/drone-tree-config /usr/local/bin
CMD /usr/local/bin/drone-tree-config
