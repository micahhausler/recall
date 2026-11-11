package main

import "errors"

// Stubs replaced as each subcommand lands.

var errNotImplemented = errors.New("not implemented yet")

func cmdLint(args []string) int { return fail(errNotImplemented) }
