#!/usr/bin/env bash

protoc -I. \
        -I/usr/local/include \
		-I$(go env GOPATH)/src \
		-I$(go env GOPATH)/pkg/mod \
		-I$(go env GOPATH)/pkg/mod/github.com/grpc-ecosystem/grpc-gateway@v1.12.1 \
		-I$(go env GOPATH)/pkg/mod/github.com/grpc-ecosystem/grpc-gateway@v1.12.1/third_party/googleapis/ \
		--go_out=plugins=grpc:. \
		--grpc-gateway_out=logtostderr=true:. \
		 storage.proto

cp storage.pb.go ../../service/storage