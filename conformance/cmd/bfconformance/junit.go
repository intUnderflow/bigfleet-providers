package main

import (
	"encoding/xml"
)

// JUnit XML structures (the subset CI systems consume).
type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

func suiteFor(name string, rs []testResult) junitSuite {
	s := junitSuite{Name: name}
	for _, r := range rs {
		c := junitCase{Name: r.Name, Classname: r.Pkg, Time: r.Elapsed}
		switch r.Outcome {
		case "fail":
			s.Failures++
			c.Failure = &junitMessage{Message: firstFailLine(r.Output), Body: r.Output}
		case "skip":
			s.Skipped++
			c.Skipped = &junitMessage{Message: firstFailLine(r.Output)}
		}
		s.Tests++
		s.Cases = append(s.Cases, c)
	}
	return s
}

// junitXML renders the baseline + extension lanes as a JUnit document.
func junitXML(provider string, base, ext []testResult) []byte {
	suites := junitSuites{Name: "bigfleet-conformance:" + provider}
	if len(base) > 0 {
		suites.Suites = append(suites.Suites, suiteFor("upstream-baseline", base))
	}
	suites.Suites = append(suites.Suites, suiteFor("extension", ext))
	for _, s := range suites.Suites {
		suites.Tests += s.Tests
		suites.Failures += s.Failures
		suites.Skipped += s.Skipped
	}
	out, _ := xml.MarshalIndent(suites, "", "  ")
	return append([]byte(xml.Header), append(out, '\n')...)
}
