package overlay

import "strings"

func splitLines(s string) []string  { return strings.Split(s, "\n") }
func splitFields(s string) []string { return strings.Fields(s) }
