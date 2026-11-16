package cli

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

type outputViewport struct {
	Width int
}

func measureOutputViewport(file *os.File, lookupEnv func(string) (string, bool)) outputViewport {
	return outputViewport{Width: measureTerminalWidth(file, lookupEnv)}
}

func measureTerminalWidth(file *os.File, lookupEnv func(string) (string, bool)) int {
	if file == nil || !term.IsTerminal(int(file.Fd())) {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err == nil && width > 0 {
		return width
	}
	if lookupEnv == nil {
		return 0
	}
	value, ok := lookupEnv("COLUMNS")
	if !ok {
		return 0
	}
	width, err = strconv.Atoi(strings.TrimSpace(value))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}
