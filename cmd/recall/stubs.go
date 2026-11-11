package main

import "errors"

// Stubs replaced as each subcommand lands.

var errNotImplemented = errors.New("not implemented yet")

func cmdRecur(args []string) int        { return fail(errNotImplemented) }
func cmdArchiveChats(args []string) int { return fail(errNotImplemented) }
func cmdEvents(args []string) int       { return fail(errNotImplemented) }
func cmdSkills(args []string) int       { return fail(errNotImplemented) }
func cmdLint(args []string) int         { return fail(errNotImplemented) }
