//go:build pgstore

package main

// durableBackends lists the durable backends compiled into this binary, reported by
// `fuse version` (mirrors the durable_backend.go / durable_backend_pg.go build-tag
// pair). The pgstore-tagged build carries both backends.
const durableBackends = "fsstore,pgstore"
