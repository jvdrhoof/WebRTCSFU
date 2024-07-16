package main

import (
	"fmt"
	"runtime"
	"strings"
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

// getCallerFunctionName retrieves the name of the function that called the log function
func getCallerFunctionName() string {
	pc, _, _, ok := runtime.Caller(2) // 2 levels up the stack
	if !ok {
		return "unknown"
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "unknown"
	}
	fullName := fn.Name()
	// Extract only the function name from the full package path
	parts := strings.Split(fullName, "/")
	return parts[len(parts)-1]
}

func (l *Logger) Log(message string, level int) {
	if l.level >= level {
		fmt.Printf("WebRTCSFU: %s: %s\n", getCallerFunctionName(), message)
	}
}

func (l *Logger) LogClient(client int, message string, level int) {
	if l.level >= level {
		fmt.Printf("WebRTCSFU [Client #%d]: %s: %s\n", client, getCallerFunctionName(), message)
	}
}

func (l *Logger) ErrorClient(client int, message string) {
	fmt.Printf("WebRTCSFU [Client #%d]: ERROR in %s: %s\n", client, getCallerFunctionName(), message)
}
