package broker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
)

// TestStop_KillsChildren проверяет, что Stop убивает не только лидера
// subprocess'а, но и его детей. Лидер здесь — обычный shell, который
// запускает sleep и сразу завершается, оставляя sleep сиротой.
func TestStop_KillsChildren(t *testing.T) {
	// sh -c '<marker>; sleep 600' — sh запишет маркер в файл, запустит
	// sleep, и (из-за exec) заменится на sleep. Значит sleep станет
	// лидером группы. Чтобы получить именно «лидер вышел, ребёнок жив»,
	// используем подshell: sh -c 'sh -c "sleep 600" ; exit 0'.
	// Внутренний sh создаст sleep как своего ребёнка, а внешний sh
	// завершится сразу.
	//
	// Чтобы убедиться, что sleep действительно жив, пишем его PID в
	// файл через pgrep.
	dir := t.TempDir()
	pidFile := dir + "/child.pid"

	script := `sh -c 'sleep 600' & echo $! > ` + pidFile + `; sleep 0.2`
	cfg := config.ServerConfig{
		Name:      "child-killer",
		Command:   "/bin/sh",
		Args:      []string{"-c", script},
		Transport: "stdio",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	p := newProcessBackend(cfg, 1024*1024, 500*time.Millisecond, logx.New(io.Discard))
	if err := p.Ensure(ctx); err != nil {
		// Expected: the shell doesn't respond to initialize.
		_ = err
	}

	// Дать скрипту время записать PID.
	time.Sleep(300 * time.Millisecond)

	// Убить группу.
	p.Stop()

	// Убедиться, что sleep мёртв.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skip("script didn't write pid file; environment may not support background sh")
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Skip("empty pid file")
	}

	// ps -p <pid> возвращает exit 0, если процесс жив.
	if err := exec.Command("kill", "-0", pid).Run(); err == nil {
		t.Fatalf("child process %s survived Stop", pid)
	}
}
