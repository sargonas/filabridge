package main

import (
	"bytes"
	"io"
	"log"
	"os"
)

// installLogSplitter routes standard-library log output by severity: lines that
// report a problem go to stderr, while informational lines go to stdout.
// Without this, the log package sends every line to stderr.
//
// Severity is inferred from the message text, which this codebase consistently
// tags ("Warning:", "Error", "Failed", ...). The log package assembles each
// entry and emits it with exactly one Write call, so classifying per Write
// reliably classifies per log line.
func installLogSplitter() {
	log.SetOutput(&severityWriter{stdout: os.Stdout, stderr: os.Stderr})
}

// severityWriter dispatches each log line to stdout or stderr based on whether
// the line contains an error/warning marker.
type severityWriter struct {
	stdout io.Writer
	stderr io.Writer
}

// errorMarkers are lowercase substrings that mark a log line as a warning or
// error. They mirror the phrasing already used across the codebase.
var errorMarkers = [][]byte{
	[]byte("warn"),
	[]byte("error"),
	[]byte("fail"),
	[]byte("could not"),
	[]byte("couldn't"),
	[]byte("unable"),
	[]byte("invalid"),
	[]byte("unexpected"),
	[]byte("reject"),
	[]byte("denied"),
	[]byte("panic"),
	[]byte("abort"),
}

func (w *severityWriter) Write(p []byte) (int, error) {
	lower := bytes.ToLower(p)
	for _, marker := range errorMarkers {
		if bytes.Contains(lower, marker) {
			return w.stderr.Write(p)
		}
	}
	return w.stdout.Write(p)
}
