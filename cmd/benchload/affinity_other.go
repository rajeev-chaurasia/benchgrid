//go:build !linux

package main

func allowedCPUs() int { return -1 }
