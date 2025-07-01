/*
Copyright 2025 Tim Hockin. All rights reserved.

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

// Package probemux is a command which serves HTTP on one port and
// forwards requests to arbitrary other ports.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"github.com/thockin/probemux/pkg/pid1"
	"github.com/thockin/probemux/pkg/version"
)

func main() {
	// In case we come up as pid 1, act as init.
	if os.Getpid() == 1 {
		fmt.Fprintf(os.Stderr, "INFO: detected pid 1, running init handler\n")
		code, err := pid1.ReRun()
		if err == nil {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "FATAL: unhandled pid1 error: %v\n", err)
		os.Exit(127)
	}

	//
	// Declare flags inside main() so they are not used as global variables.
	//

	flVersion := pflag.Bool("version", false, "print the version and exit")
	flHelp := pflag.BoolP("help", "h", false, "print help text and exit")
	pflag.BoolVarP(flHelp, "__?", "?", false, "") // support -? as an alias to -h
	mustMarkHidden("__?")
	flManual := pflag.Bool("man", false, "print the full manual and exit")
	flVerbose := pflag.IntP("verbose", "v", 0, "logs at this V level and lower will be printed")

	flPort := pflag.Int("port", 0, "the port to listen on (required)")
	flPprof := pflag.Bool("pprof", false, "enable the pprof debug endpoints at /debug/pprof/...")
	flTimeout := pflag.Duration("timeout", 3*time.Second, "the timeout for backend probes")

	//
	// Parse and verify flags.  Errors here are fatal.
	//

	// For whatever reason pflag hardcodes stderr for the "usage" line when
	// using the default FlagSet.  We tweak the output a bit anyway.
	usage := func(out io.Writer, msg string) {
		// When pflag parsing hits an error, it prints a message before and
		// after the usage, which makes for nice reading.
		if msg != "" {
			fmt.Fprintln(out, msg)
		}
		fmt.Fprintf(out, "Usage: %s [OPTIONS]... BACKENDS...\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, " OPTIONS:")
		pflag.CommandLine.SetOutput(out)
		pflag.PrintDefaults()
		if msg != "" {
			fmt.Fprintln(out, msg)
		}
	}
	pflag.Usage = func() { usage(os.Stderr, "") }
	pflag.Parse()

	// Handle print-and-exit cases.
	if *flVersion {
		fmt.Fprintln(os.Stdout, version.Version)
		os.Exit(0)
	}
	if *flHelp {
		usage(os.Stdout, "")
		os.Exit(0)
	}
	if *flManual {
		printManPage()
		os.Exit(0)
	}

	// Make sure we have a root dir in which to work.
	if *flPort == 0 {
		usage(os.Stderr, "required flag: --port must be specified")
		os.Exit(1)
	}
	if *flPort < 0 || *flPort > 65535 {
		usage(os.Stderr, "invalid flag: --port must be a valid port number [1-65535]")
		os.Exit(1)
	}

	// Initialize logging.
	logopts := funcr.Options{
		LogCaller:    funcr.All,
		LogTimestamp: true,
		Verbosity:    *flVerbose,
	}
	log := funcr.NewJSON(func(obj string) { fmt.Fprintln(os.Stderr, obj) }, logopts)

	//
	// From here on, output goes through logging.
	//

	log.V(0).Info("starting up",
		"version", version.Version,
		"pid", os.Getpid(),
		"uid", os.Getuid(),
		"gid", os.Getgid(),
		"home", os.Getenv("HOME"),
		"flags", flagsForLog(*flVerbose),
		"backends", pflag.Args())

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *flPort))
	if err != nil {
		log.Error(err, "can't listen", "port", *flPort)
		os.Exit(1)
	}
	router := http.NewServeMux()

	// This is the main probe multiplexer.
	client := http.Client{
		Timeout: *flTimeout,
	}
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { probeMux(w, r, &client, log) })

	// This is a trivial liveness check endpoint.
	router.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {})

	// Serve prometheus metrics.
	router.Handle("/metrics", promhttp.Handler())

	if *flPprof {
		// Serve profiling info.
		router.HandleFunc("/debug/pprof/", pprof.Index)
		router.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		router.HandleFunc("/debug/pprof/profile", pprof.Profile)
		router.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		router.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	log.Info("serving HTTP", "port", *flPort)
	if err := http.Serve(ln, router); err != nil {
		log.Error(err, "HTTP server terminated")
		os.Exit(1)
	}
}

// mustMarkHidden is a helper around pflag.CommandLine.MarkHidden.
// It panics if there is an error (as these indicate a coding issue).
// This makes it easier to keep the linters happy.
func mustMarkHidden(name string) {
	err := pflag.CommandLine.MarkHidden(name)
	if err != nil {
		panic(fmt.Sprintf("error marking flag %q as hidden: %v", name, err))
	}
}

// flagsForLog visit all flags and decides what to log. This returns a slice
// rather than a map so it is always sorted.
func flagsForLog(v int) []string {
	ret := []string{}
	pflag.VisitAll(func(fl *pflag.Flag) {
		// Don't log hidden flags
		if fl.Hidden {
			return
		}
		// Don't log unchanged values
		if !fl.Changed && v <= 3 {
			return
		}

		arg := fl.Name
		val := fl.Value.String()
		if fl.Value.Type() == "string" {
			val = "'" + val + "'"
		}

		// Don't log empty, unchanged values
		if val == "" && !fl.Changed && v < 6 {
			return
		}

		ret = append(ret, fmt.Sprintf("--%s=%s", arg, val))
	})
	return ret
}

var (
	metricProbeCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "probe_count_total",
		Help: "How many probes have completed, partitioned by state (success, failure, error)",
	}, []string{"status"})
	metricProbeDuration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "probe_duration_seconds",
		Help: "Summary of probe durations, partitioned by state (success, failure, error)",
	}, []string{"status"})
)

const (
	muxStatusSuccess = "success"
	muxStatusFailure = "failure"
	muxStatusError   = "error"
)

func init() {
	prometheus.MustRegister(metricProbeCount)
	prometheus.MustRegister(metricProbeDuration)
}

func probeMux(w http.ResponseWriter, r *http.Request, client *http.Client, log logr.Logger) {
	log.Info("HTTP probe request", "clientAddr", r.RemoteAddr)
	code, jb := probeBackends(client, log)
	jb = append(jb, '\n')

	w.WriteHeader(code)
	w.Header().Add("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(jb); err != nil {
		log.Error(err, "failed to write HTTP response", "status", code)
	}
}

// probeBackends probes all the configured backends and returns the HTTP status
// code and body.
func probeBackends(client *http.Client, log logr.Logger) (int, []byte) {
	type backendResult struct {
		URL    string `json:"url"`
		Status int    `json:"status,omitempty"`
		Body   string `json:"body,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	type muxResponse struct {
		Results []backendResult `json:"results,omitempty"`
		Error   string          `json:"error,omitempty"`
	}

	start := time.Now()
	mxr := muxResponse{}

	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	compositeStatus := muxStatusSuccess

	for _, url := range pflag.Args() {
		// Run all backend probes in parallel.
		wg.Add(1)
		go func() {
			defer wg.Done()

			ber := backendResult{URL: url}
			status := muxStatusSuccess

			resp, err := client.Get(url)
			if err != nil {
				log.Error(err, "HTTP error from backend", "url", url)
				status = muxStatusError
				ber.Error = err.Error()
			} else {
				defer resp.Body.Close()
				st := resp.StatusCode
				log.Info("backend result", "url", url, "status", st)
				ber.Status = st
				if st < 200 || st > 299 {
					status = muxStatusFailure
				}
				if body, err := io.ReadAll(resp.Body); err != nil {
					status = muxStatusError
					ber.Error = err.Error()
				} else {
					ber.Body = string(body)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if status == muxStatusError {
				// Errors take precedence over everything else because
				// SOMETHING is going wrong.
				compositeStatus = status
			} else if status == muxStatusFailure && compositeStatus == muxStatusSuccess {
				// One backend failure == a composite failure.
				compositeStatus = status
			}
			mxr.Results = append(mxr.Results, ber)
		}()
	}
	wg.Wait()

	jb, err := json.MarshalIndent(mxr, "", "  ")
	if err != nil {
		// Something went seriously wrong.
		compositeStatus = muxStatusError
		jb = fmt.Appendf([]byte{}, "{\n  \"error\": %q\n}", err.Error())
	}

	metricProbeCount.WithLabelValues(compositeStatus).Inc()
	metricProbeDuration.WithLabelValues(compositeStatus).Observe(time.Since(start).Seconds())

	muxCode := 0
	switch compositeStatus {
	case muxStatusSuccess:
		muxCode = http.StatusOK
	case muxStatusFailure:
		muxCode = http.StatusServiceUnavailable
	case muxStatusError:
		muxCode = http.StatusBadGateway
	default:
		log.Error(nil, "unknown mux status", "status", compositeStatus)
		muxCode = http.StatusInternalServerError
	}

	log.Info("probe result", "status", compositeStatus, "code", muxCode)

	return muxCode, jb
}

// This string is formatted for 80 columns.  Please keep it that way.
// DO NOT USE TABS.
var manual = `
PROBEMUX

NAME
    probemux - multiplex many HTTP probes into one.

SYNOPSIS
    probemux --port=<port> [OPTIONS]... BACKENDS...

DESCRIPTION

    When the / URL is read, execute one HTTP GET operation against each backend
    URL and return the composite result.

    If all backends return a 2xx HTTP status, this will respond with 200 "OK".
    If all backends return valid HTTP responses, but any backend returns a
    non-2xx status, this will respond with 503 "Service Unavailable". If any
    backend produces an HTTP error, this will respond with 502 "Bad Gateway".

    Backends are probed synchronously when an incoming request is received, but
    backends may be probed in parallel to each other.

OPTIONS

    Probemux has exactly one required flag.

    --port
            The port number on which to listen. Probemux listens on the
            unspecified address (all IPs, all families).

    All other flags are optional.

    -?, -h, --help
            Print help text and exit.

    --man
            Print this manual and exit.

    --pprof
            Enable the pprof debug endpoints on probemux's port at
            /debug/pprof/...

    --timeout <duration>
            The time allowed for each backend to respond, formatted as a
            Go-style duration string. If not specified this defaults to 3
            seconds (3s).

    -v, --verbose <int>, $GITSYNC_VERBOSE
            Set the log verbosity level.  Logs at this level and lower will be
            printed.

    --version
            Print the version and exit.

EXAMPLE USAGE

    probemux \
        --port=9376 \
        --timeout=5s \
        http://localhost:1234/healthz \
        http://localhost:1234/another \
        http://localhost:5678/a-third
`

func printManPage() {
	fmt.Fprint(os.Stdout, manual)
}
