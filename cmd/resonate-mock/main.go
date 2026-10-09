// resonate-mock serves a fake GitLab and five fake sites for local demos.
// Pair it with sites.demo.json (make demo imports it):
//
//	go run ./cmd/resonate-mock
//	curl -X POST localhost:9999/mock/push                 # merge a commit to main
//	curl -X POST localhost:9999/mock/sites/makassar/toggle # bring a site up/down
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/aufarsukmajaya/resonate/internal/mock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address")
	delay := flag.Duration("delay", 4*time.Second, "how long a fake pipeline runs")
	flag.Parse()

	m := mock.New()
	m.Delay = *delay
	log.Printf("resonate-mock on http://%s (sites: %v)", *addr, mock.SiteNames)
	srv := &http.Server{Addr: *addr, Handler: m, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
