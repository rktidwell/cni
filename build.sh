#!/bin/bash 
protoc -I libcni/ \
    -I${GOPATH}/src \
    -I${GOPATH}/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
    --go_out=plugins=grpc:libcni \
    libcni/cnigrpc.proto
go build -i -v -o cnitool/cnitool cnitool/cnitool.go 
go build -i -v -o cniserver/server cniserver/server.go
#go build -i -v -o cnigrpc/bin/client cnigrpc/client/client.go
