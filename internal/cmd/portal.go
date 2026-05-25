package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const portalPollInterval = 2 * time.Second

type portalInstance struct {
	Name       string `json:"name"`
	SocketPath string `json:"socketPath"`
}

// NewPortalCmd creates the portal command.
func NewPortalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "portal",
		Short: "Serve a local portal for current Sandman runs",
		Long:  "Serve a localhost portal for the current repository and poll .sandman/runs for live Sandman instances.",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			repoRoot, err := findRepoRoot(cwd)
			if err != nil {
				return err
			}

			ctx, stop := signalContext(cmd.Context())
			defer stop()

			return runPortalServer(ctx, repoRoot, port, cmd.OutOrStdout())
		},
	}

	cmd.Flags().Int("port", 5000, "Port to bind on 127.0.0.1")
	return cmd
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
		close(sigCh)
	}()
	return ctx, cancel
}

func runPortalServer(ctx context.Context, repoRoot string, port int, out io.Writer) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind portal on 127.0.0.1:%d: %w", port, err)
	}
	defer listener.Close()

	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	actualPort := port
	if tcpAddr != nil {
		actualPort = tcpAddr.Port
	}

	if _, err := fmt.Fprintf(out, "Portal listening on http://127.0.0.1:%d\n", actualPort); err != nil {
		return fmt.Errorf("write portal address: %w", err)
	}

	server := &http.Server{Handler: newPortalHandler(repoRoot)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve portal: %w", err)
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve portal: %w", err)
	}
}

func newPortalHandler(repoRoot string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/instances", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		instances, err := discoverPortalInstances(repoRoot)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repoRoot":  repoRoot,
			"instances": instances,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data := struct {
			RepoRoot      string
			PollInterval  int
			InstancesPath string
		}{
			RepoRoot:      repoRoot,
			PollInterval:  int(portalPollInterval / time.Millisecond),
			InstancesPath: "/api/instances",
		}
		_ = portalPageTemplate.Execute(w, data)
	})
	return mux
}

func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if hasGitMarker(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("locate repository root from %s: .git not found", start)
}

func hasGitMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func discoverPortalInstances(repoRoot string) ([]portalInstance, error) {
	runsDir := filepath.Join(repoRoot, ".sandman", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}

	instances := make([]portalInstance, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := filepath.Join(runsDir, entry.Name(), "run.sock")
		info, err := os.Lstat(sockPath)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		instances = append(instances, portalInstance{Name: entry.Name(), SocketPath: sockPath})
	}

	sort.Slice(instances, func(i, j int) bool {
		return strings.Compare(instances[i].Name, instances[j].Name) < 0
	})
	return instances, nil
}

var portalPageTemplate = template.Must(template.New("portal").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sandman Portal</title>
  <style>
    body { font-family: system-ui, sans-serif; margin: 2rem; color: #111827; background: #f9fafb; }
    header { margin-bottom: 1.5rem; }
    code { background: #e5e7eb; padding: 0.1rem 0.3rem; border-radius: 0.25rem; }
    table { border-collapse: collapse; width: 100%; max-width: 64rem; background: white; }
    th, td { text-align: left; padding: 0.75rem; border-bottom: 1px solid #e5e7eb; }
    th { background: #f3f4f6; }
    .muted { color: #6b7280; }
  </style>
</head>
<body>
  <header>
    <h1>Sandman Portal</h1>
    <div class="muted">Repo: <code>{{.RepoRoot}}</code></div>
    <div class="muted">Polls <code>{{.InstancesPath}}</code> every <code>{{.PollInterval}}ms</code>.</div>
  </header>
  <table>
    <thead><tr><th>Run</th><th>Socket</th></tr></thead>
    <tbody id="runs"><tr><td colspan="2" class="muted">Loading...</td></tr></tbody>
  </table>
  <script>
    const runsPath = {{printf "%q" .InstancesPath}};
    const tbody = document.getElementById('runs');
    async function refresh() {
      const res = await fetch(runsPath, {cache: 'no-store'});
      const data = await res.json();
      tbody.innerHTML = '';
      if (!data.instances.length) {
        tbody.innerHTML = '<tr><td colspan="2" class="muted">No Sandman runs found.</td></tr>';
        return;
      }
      for (const inst of data.instances) {
        const row = document.createElement('tr');
        row.innerHTML = '<td>' + inst.name + '</td><td><code>' + inst.socketPath + '</code></td>';
        tbody.appendChild(row);
      }
    }
    refresh();
    setInterval(refresh, {{.PollInterval}});
  </script>
</body>
</html>`))
