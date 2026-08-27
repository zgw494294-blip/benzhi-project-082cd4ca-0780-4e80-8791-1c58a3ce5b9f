package main

import (
	"io"
	"testing"
)

func TestParseConfigAddressPrecedence(t *testing.T) {
	getenv := func(name string) string {
		if name == "PORT" {
			return "19444"
		}
		return ""
	}
	cfg, err := parseConfig(nil, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != "127.0.0.1:19444" {
		t.Fatalf("PORT 未绑定回环地址: %s", cfg.address)
	}
	cfg, err = parseConfig([]string{"-addr=127.0.0.1:19555"}, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != "127.0.0.1:19555" {
		t.Fatalf("显式 addr 应优先: %s", cfg.address)
	}
}

func TestParseConfigRejectsInvalidPort(t *testing.T) {
	_, err := parseConfig(nil, func(string) string { return "8080x" }, io.Discard)
	if err == nil {
		t.Fatal("非数字 PORT 不应被接受")
	}
}
