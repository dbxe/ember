package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHomesMissingFileUsesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewAccountManager()
	accounts, err := m.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	want := []Account{{Name: "work", Home: filepath.Join(home, ".codex")}}
	if !reflect.DeepEqual(accounts, want) {
		t.Fatalf("accounts = %+v, want %+v", accounts, want)
	}
	if _, err := os.Stat(m.homesPath); !os.IsNotExist(err) {
		t.Fatalf("default must not create homes.json: %v", err)
	}
}

func TestHomesExpandsTildeAndPreservesOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewAccountManager()
	writeTestFile(t, m.homesPath, `[
		{"name":"work","home":"~/.codex"},
		{"name":"main","home":"~/.codex_main"},
		{"name":"root","home":"~"},
		{"name":"absolute","home":"/configured/codex home"}
	]`)
	accounts, err := m.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	want := []Account{
		{Name: "work", Home: filepath.Join(home, ".codex")},
		{Name: "main", Home: filepath.Join(home, ".codex_main")},
		{Name: "root", Home: home},
		{Name: "absolute", Home: "/configured/codex home"},
	}
	if !reflect.DeepEqual(accounts, want) {
		t.Fatalf("accounts = %+v, want %+v", accounts, want)
	}
	writeTestFile(t, m.homesPath, `[]`)
	accounts, err = m.ListAccounts()
	if err != nil || len(accounts) != 0 {
		t.Fatalf("empty configuration = %+v, %v", accounts, err)
	}
}

func TestMalformedHomesIsVisibleMenuError(t *testing.T) {
	for _, contents := range []string{
		`[`, `null`, `{}`, `[null]`,
		`[{"name":"work"}]`,
		`[{"name":"","home":"~/.codex"}]`,
		`[{"name":"work","home":"relative"}]`,
		`[{"name":"work","home":7}]`,
		`[{"name":"work","home":"~/.codex"},{"name":"work","home":"~/.codex_main"}]`,
	} {
		t.Run(contents, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			a := NewApp()
			writeTestFile(t, a.manager.homesPath, contents)
			a.HandleMenuAction(TagRefresh)
			if !strings.Contains(a.lastError, "homes.json:") {
				t.Fatalf("configuration error = %q", a.lastError)
			}
			for _, entry := range buildMenuEntries(a.accounts, a.lastError) {
				if strings.Contains(entry.Title, "homes.json:") {
					return
				}
			}
			t.Fatal("menu must show the configuration error")
		})
	}
}
