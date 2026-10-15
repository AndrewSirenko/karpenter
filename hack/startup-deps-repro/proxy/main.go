/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig of the real apiserver")
	listen := flag.String("listen", "127.0.0.1:18443", "plain http listen address")
	match := flag.String("match", "", "regexp on URL path; matching GET (list and watch) requests are delayed")
	delay := flag.Duration("delay", 0, "delay applied to matching requests")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		log.Fatal(err)
	}
	target, err := url.Parse(cfg.Host)
	if err != nil {
		log.Fatal(err)
	}
	re := regexp.MustCompile(*match)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.FlushInterval = -1

	log.Printf("proxying %s -> %s, delaying GET %q by %s", *listen, cfg.Host, *match, *delay)
	log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *match != "" && r.Method == http.MethodGet && re.MatchString(r.URL.Path) {
			log.Printf("delaying %s", r.URL.String())
			select {
			case <-time.After(*delay):
			case <-r.Context().Done():
				log.Printf("client gave up on %s", r.URL.String())
				return
			}
			log.Printf("releasing %s", r.URL.String())
		}
		proxy.ServeHTTP(w, r)
	})))
}
