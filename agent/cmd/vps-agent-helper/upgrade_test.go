package main

import "testing"

func TestValidReleaseTag(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "v0.3.8", want: true},
		{value: "v10.20.30", want: true},
		{value: "0.3.8", want: false},
		{value: "v0.3", want: false},
		{value: "v0.3.8-rc1", want: false},
		{value: "v0.x.8", want: false},
	} {
		if got := validReleaseTag(test.value); got != test.want {
			t.Fatalf("validReleaseTag(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  vps-agent-linux-amd64\ninvalid  ignored\n")
	checksums, err := parseChecksums(data)
	if err != nil {
		t.Fatalf("parseChecksums: %v", err)
	}
	if checksums["vps-agent-linux-amd64"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected checksum map: %#v", checksums)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	older, _ := parseReleaseVersion("v0.3.7")
	current, _ := parseReleaseVersion("v0.3.8")
	newer, _ := parseReleaseVersion("v0.4.0")
	if compareReleaseVersions(older, current) >= 0 {
		t.Fatal("older release was not ordered before current")
	}
	if compareReleaseVersions(current, current) != 0 {
		t.Fatal("equal releases did not compare equal")
	}
	if compareReleaseVersions(newer, current) <= 0 {
		t.Fatal("newer release was not ordered after current")
	}
}
