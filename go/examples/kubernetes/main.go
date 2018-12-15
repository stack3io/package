package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/metaparticle-io/package/go/metaparticle"
)

const Dockerfile = `FROM golang:1.9 as builder
WORKDIR /go/src/app
COPY . .
RUN go get -u github.com/golang/dep/cmd/dep && dep init && CGO_ENABLED=0 GOOS=linux go-wrapper install
FROM alpine:3.7
RUN apk add --no-cache --update ca-certificates
COPY --from=builder /go/bin/app .
LABEL wooo=woooo
CMD ["./app"]
`

var port int32 = 9090

func main() {
	metaparticle.Containerize(
		&metaparticle.Runtime{
			Executor:      "kubernetes",
			Replicas:      2,
			Ports:         []int32{port},
			PublicAddress: true,
		},
		&metaparticle.Package{
			Builder:    "docker",
			Repository: "docker.io/ultimateboy",
			Name:       "go-simple",
			Publish:    true,
			Dockerfile: Dockerfile,
		},
		run,
	)
}

func run() {
	fmt.Printf("Serving on %d...\n", port)
	http.HandleFunc("/", sayhello)
	err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func sayhello(w http.ResponseWriter, r *http.Request) {
	log.Println("new request!")
	fmt.Fprintf(w, "Hello!")
}
