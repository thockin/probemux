# probemux

probemux is a simple command which receives incoming HTTP probe requests (as in
a Kubernetes liveness probe) and probes a set of "backends", multiplexing the
results.

## Building it

We use [docker buildx](https://github.com/docker/buildx) to build images.

```
# build the container
make container REGISTRY=registry VERSION=tag
```

```
# build the container behind a proxy
make container REGISTRY=registry VERSION=tag \
    HTTP_PROXY=http://<proxy_address>:<proxy_port> \
    HTTPS_PROXY=https://<proxy_address>:<proxy_port>
```

```
# build the container for an OS/arch other than the current (e.g. you are on
# MacOS and want to run on Linux)
make container REGISTRY=registry VERSION=tag \
    GOOS=linux GOARCH=amd64
```

## Manual

```
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
    backend produced an HTTP error, this will respond with 502 "Bad Gateway". 

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
```
