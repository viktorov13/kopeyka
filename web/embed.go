// Package web contains the buildless web application.
package web

import "embed"

// FS contains the application's static assets.
//
//go:embed static/*
var FS embed.FS
