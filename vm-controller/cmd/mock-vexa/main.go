// Command mock-vexa runs an in-memory imitation of the Vexa-VM API for
// trying vm-controller without a hypervisor. It is a development tool and is
// not part of the release build.
//
//	go run ./cmd/mock-vexa -listen 127.0.0.1:8080 -api-key vxa_dev \
//	    -vm 3f1c9a2e-5b7d-4e8f-9a0b-1c2d3e4f5a61=minecraft=stopped
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/vm-controller/internal/vexatest"
)

type vmFlags []string

func (v *vmFlags) String() string     { return strings.Join(*v, ",") }
func (v *vmFlags) Set(s string) error { *v = append(*v, s); return nil }

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on (keep it on loopback)")
	key := flag.String("api-key", os.Getenv("VEXA_API_KEY"), "bearer key the mock accepts (default: $VEXA_API_KEY)")
	startDelay := flag.Duration("start-delay", 4*time.Second, "how long a start job takes")
	shutdownDelay := flag.Duration("shutdown-delay", 20*time.Second, "how long until a gracefully stopped VM is reported as stopped")
	var vms vmFlags
	flag.Var(&vms, "vm", "VM as id=name=state (repeatable; state: running, stopped, paused, ...)")
	flag.Parse()

	if *key == "" {
		log.Fatal("set -api-key or VEXA_API_KEY")
	}
	if len(vms) == 0 {
		vms = vmFlags{
			"3f1c9a2e-5b7d-4e8f-9a0b-1c2d3e4f5a61=minecraft-1=stopped",
			"8d2e4b6a-1c3f-4a5b-8c7d-9e0f1a2b3c42=minecraft-2=running",
		}
	}
	mock := vexatest.NewHandler(*key)
	mock.StartDelay = *startDelay
	mock.ShutdownDelay = *shutdownDelay
	for _, v := range vms {
		parts := strings.SplitN(v, "=", 3)
		if len(parts) != 3 {
			log.Fatalf("invalid -vm %q, want id=name=state", v)
		}
		mock.AddVM(parts[0], parts[1], parts[2])
		fmt.Printf("mock VM %s (%s): %s\n", parts[0], parts[1], parts[2])
	}
	fmt.Printf("mock Vexa-VM API listening on http://%s\n", *listen)
	srv := &http.Server{Addr: *listen, Handler: mock, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
