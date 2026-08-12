// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every shipped blank must stay parseable — this is the drift gate for
// config.example.json and examples/.
func TestShippedConfigBlanksParse(t *testing.T) {
	files, err := filepath.Glob("examples/*.json")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "config.example.json")
	if len(files) < 4 {
		t.Fatalf("expected the example blanks, found only %v", files)
	}
	for _, f := range files {
		if _, err := loadFileConfig(f, true); err != nil {
			t.Errorf("%s does not parse: %v", f, err)
		}
	}
}

func TestStripCommentsKeepsLinesAndStrings(t *testing.T) {
	in := "// top comment\n{\n  // inner\n  \"base_url\": \"https://x//y\"\n}\n"
	out := string(stripComments([]byte(in)))
	if strings.Contains(out, "top comment") || strings.Contains(out, "inner") {
		t.Fatalf("comments survived: %q", out)
	}
	if !strings.Contains(out, "https://x//y") {
		t.Fatalf("string content mangled: %q", out)
	}
	if strings.Count(in, "\n") != strings.Count(out, "\n") {
		t.Fatal("line count changed — JSON error line numbers would drift")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"listen": ":1", "nope": true}`), 0o600)
	if _, err := loadFileConfig(path, true); err == nil {
		t.Fatal("unknown key must be rejected")
	}
}

func TestMissingFileOnlyFatalWhenExplicit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := loadFileConfig(missing, false); err != nil {
		t.Fatalf("implicit missing config must be fine, got %v", err)
	}
	if _, err := loadFileConfig(missing, true); err == nil {
		t.Fatal("explicit missing config must error")
	}
}

func TestSecretResolutionFileThenEnv(t *testing.T) {
	c := fileConfig{}
	c.Email.SMTPPass = "from-file"
	if got := c.smtpPass(); got != "from-file" {
		t.Fatalf("file credential ignored: %q", got)
	}
	c.Email.SMTPPass = ""
	c.Email.SMTPPassEnv = "STATTII_TEST_PASS"
	t.Setenv("STATTII_TEST_PASS", "from-env")
	if got := c.smtpPass(); got != "from-env" {
		t.Fatalf("env fallback broken: %q", got)
	}
	c.Telegram.TokenEnv = "STATTII_TEST_TG"
	t.Setenv("STATTII_TEST_TG", "tg-token")
	if got := c.telegramToken(); got != "tg-token" {
		t.Fatalf("telegram env fallback broken: %q", got)
	}
}
