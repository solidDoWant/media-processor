package main

import (
	"fmt"
	"slices"
	"strings"
)

// allToken expands to every entry in the known list when encountered in a
// WORKER_ACTIVITIES list.
const allToken = "all"

// resolveActivities evaluates the WORKER_ACTIVITIES tokens left-to-right
// against an initially empty set, returning the final set in canonical order
// (matching the order in known).
//
// Token grammar:
//   - "all"   sets the working set to every known token
//   - "name"  adds that token to the set
//   - "!name" removes that token from the set
//
// Errors:
//   - any token (with or without the "!" prefix) that is not in known
//   - a final empty set
func resolveActivities(tokens, known []string) ([]string, error) {
	set := map[string]struct{}{}

	for _, raw := range tokens {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}

		if token == allToken {
			for _, k := range known {
				set[k] = struct{}{}
			}

			continue
		}

		negate := false

		name := token
		if strings.HasPrefix(name, "!") {
			negate = true
			name = strings.TrimSpace(name[1:])
		}

		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("unknown WORKER_ACTIVITIES token %q; known: %v", token, known)
		}

		if negate {
			delete(set, name)
		} else {
			set[name] = struct{}{}
		}
	}

	if len(set) == 0 {
		return nil, fmt.Errorf("WORKER_ACTIVITIES resolved to empty set")
	}

	resolved := make([]string, 0, len(set))

	for _, k := range known {
		if _, ok := set[k]; ok {
			resolved = append(resolved, k)
		}
	}

	return resolved, nil
}

// parseWorkerActivities splits a comma-separated WORKER_ACTIVITIES env-var
// value into tokens. Empty input yields a single "all" token so an unset
// variable behaves the same as an explicit "all".
func parseWorkerActivities(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{allToken}
	}

	parts := strings.Split(raw, ",")
	tokens := make([]string, 0, len(parts))

	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		return []string{allToken}
	}

	return tokens
}
