package main

// 以下变量在构建时可通过 -ldflags 注入，例如：
// go build -ldflags "-X main.Version=v0.1.0 -X main.Commit=abc1234 -X main.BuildTime=2026-11-24T00:00:00Z"
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = ""
)
