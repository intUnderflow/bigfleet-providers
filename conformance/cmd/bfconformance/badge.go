package main

import (
	"encoding/json"
	"fmt"
)

// shieldsBadge is the schema of a shields.io "endpoint" badge, so a README can
// render the live certification status with:
//
//	https://img.shields.io/endpoint?url=<raw url to badge.json>
type shieldsBadge struct {
	SchemaVersion int    `json:"schemaVersion"`
	Label         string `json:"label"`
	Message       string `json:"message"`
	Color         string `json:"color"`
}

// badgeFor renders a certification badge from a report: the verdict plus the
// passed/applicable behavior count, coloured by verdict.
func badgeFor(rep report) shieldsBadge {
	applicable := rep.Summary.Passed + rep.Summary.Failed + rep.Summary.Skipped
	color := "brightgreen"
	switch rep.Verdict {
	case "FAILED":
		color = "red"
	case "INCOMPLETE":
		color = "yellow"
	}
	return shieldsBadge{
		SchemaVersion: 1,
		Label:         "bigfleet conformance",
		Message:       fmt.Sprintf("%s (%d/%d)", rep.Verdict, rep.Summary.Passed, applicable),
		Color:         color,
	}
}

func badgeJSON(rep report) []byte {
	b, _ := json.MarshalIndent(badgeFor(rep), "", "  ")
	return append(b, '\n')
}
