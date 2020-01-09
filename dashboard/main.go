package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	http.Handle("/", http.FileServer(http.Dir("web")))
	fmt.Println("starting at http://dashboard.localhost")
	log.Fatal(http.ListenAndServe(":http", nil))
}
