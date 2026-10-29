//go:build linux

package main

import (
	"runtime"

	"github.com/MahyarNemati/mini-container/internal/overlay"
)

func overlaySupported() error { return overlay.Supported() }

func runtimeDescription() string { return runtime.GOOS + "/" + runtime.GOARCH }
