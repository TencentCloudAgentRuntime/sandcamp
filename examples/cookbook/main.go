package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const defaultCommand = "inspect"

func main() {
	command, err := parseCommand(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		writeUsage(os.Stderr)
		os.Exit(2)
	}
	if command == "help" {
		writeUsage(os.Stdout)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, command); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseCommand(arguments []string) (string, error) {
	if len(arguments) == 0 {
		return defaultCommand, nil
	}
	if len(arguments) != 1 {
		return "", fmt.Errorf("只接受一个命令")
	}
	switch arguments[0] {
	case "inspect", "requests", "smoke", "help", "-h", "--help":
		if arguments[0] == "-h" || arguments[0] == "--help" {
			return "help", nil
		}
		return arguments[0], nil
	default:
		return "", fmt.Errorf("未知命令 %q", arguments[0])
	}
}

func writeUsage(output io.Writer) {
	fmt.Fprintln(output, "usage: go run ./examples/cookbook [inspect|requests|smoke]")
	fmt.Fprintln(output, "  inspect   解码并展示 RenderMounts/RenderStart 输出（默认）")
	fmt.Fprintln(output, "  requests  展示完整的 Tool 与 Instance 请求")
	fmt.Fprintln(output, "  smoke     创建临时 AGS Tool/Instance，验证后自动清理")
}

func run(ctx context.Context, command string) error {
	rendered, err := buildCookbook(cookbookImageReferences())
	if err != nil {
		return err
	}
	switch command {
	case "inspect":
		return writeInspection(rendered)
	case "requests":
		return writeRequests(rendered)
	case "smoke":
		return runSmoke(ctx, rendered)
	default:
		return fmt.Errorf("不支持命令 %q", command)
	}
}
