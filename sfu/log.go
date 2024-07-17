package main

import (
	"fmt"
)

const (
	LevelDefault = 0
	LevelVerbose = 1
	LevelDebug   = 2
)

// Logger struct with current log level
type Logger struct {
	level int
}

// NewLogger creates a new logger with the given log level
func NewLogger(level int) *Logger {
	return &Logger{level: level}
}

func (l *Logger) Log(function string, message string, level int) {
	if l.level >= level {
		fmt.Printf("WebRTCSFU: %s: %s\n", function, message)
	}
}

func (l *Logger) LogClient(client int, function string, message string, level int) {
	if l.level >= level {
		fmt.Printf("WebRTCSFU [Client #%d]: %s: %s\n", client, function, message)
	}
}

func (l *Logger) ErrorClient(client int, function string, message string) {
	fmt.Printf("WebRTCSFU [Client #%d]: ERROR in %s: %s\n", client, function, message)
}
