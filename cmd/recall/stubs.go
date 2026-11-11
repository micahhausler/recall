package main

import "errors"

// Stubs replaced as each subcommand lands.

var errNotImplemented = errors.New("not implemented yet")

func cmdEvents(args []string) int { return fail(errNotImplemented) }
func cmdSkills(args []string) int { return fail(errNotImplemented) }
func cmdLint(args []string) int   { return fail(errNotImplemented) }
