// Command replaygate checks a pull request against recorded traffic. It is
// what the GitHub Action runs, and you can run it by hand too.
//
// Usage:
//
//	replaygate run
//
// It replays the project's recorded requests against the baseline build and
// the pull request build, compares the answers, prints a summary, and on a
// pull request posts the report as a comment.
//
// Everything is configured through environment variables (see .env.example).
//
// Exit status: 0 the check passed, 1 it found a regression, 2 it could not
// reach a verdict (bad configuration, API unreachable, baseline broken, timeout).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sangsty/replaygate/internal/gate"
	"github.com/Sangsty/replaygate/internal/github"
	"github.com/Sangsty/replaygate/internal/report"
)

const (
	exitPassed  = 0
	exitFailed  = 1
	exitErrored = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] != "run" {
		fmt.Fprintf(stderr, "usage: replaygate run   (configured through environment variables)\n")
		return exitErrored
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	opts, post, err := loadOptions(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "replaygate: %v\n", err)
		return exitErrored
	}
	opts.Log = log

	res, err := gate.Run(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "replaygate: the check could not be completed: %v\n", err)
		writeOutputs(getenv, log, report.Result{Run: report.Run{Status: report.StatusErrored}})
		return exitErrored
	}

	fmt.Fprint(stdout, report.Text(res))
	md := report.Markdown(res, report.Options{RunURL: post.runURL(res.Run.ID)})
	if path := getenv("GITHUB_STEP_SUMMARY"); path != "" {
		appendFile(log, path, md+"\n")
	}
	if path := getenv("RG_REPORT_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			log.Warn("could not write the report file", "err", err)
		}
	}
	writeOutputs(getenv, log, res)
	post.comment(ctx, log, opts, md)

	switch res.Run.Status {
	case report.StatusPassed:
		return exitPassed
	case report.StatusFailed:
		return exitFailed
	default:
		return exitErrored
	}
}

// poster knows where the report goes.
type poster struct {
	dashboardURL, projectID string
	client                  *github.Client
	repo                    string
}

func (p poster) runURL(runID string) string {
	if p.dashboardURL == "" || runID == "" {
		return ""
	}
	return p.dashboardURL + "/p/" + p.projectID + "/runs/" + runID
}

// comment posts md on the pull request. Failing to do so is a warning: the
// verdict is in the exit status and in the job summary either way, and pull
// requests from forks legitimately get a read-only token.
func (p poster) comment(ctx context.Context, log *slog.Logger, o gate.Options, md string) {
	if p.client == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u, err := p.client.UpsertComment(cctx, p.repo, *o.PRNumber, report.Marker, md)
	if err != nil {
		log.Warn("could not post the pull request comment (the check result is unaffected)", "err", err,
			"hint", "the workflow needs permissions: pull-requests: write, and PRs from forks get a read-only token")
		return
	}
	log.Info("pull request comment posted", "url", u)
}

func loadOptions(getenv func(string) string) (gate.Options, poster, error) {
	req, err := required(getenv, "RG_API_URL", "RG_PROJECT_ID", "RG_API_KEY", "RG_BASELINE_URL", "RG_CANDIDATE_URL")
	if err != nil {
		return gate.Options{}, poster{}, err
	}
	apiURL, projectID, apiKey, baseline, candidate := req[0], req[1], req[2], req[3], req[4]

	o := gate.Options{APIURL: apiURL, ProjectID: projectID, APIKey: apiKey, ReplayAuth: getenv("RG_REPLAY_AUTH")}
	if _, err = absURL("RG_API_URL", apiURL); err != nil {
		return o, poster{}, err
	}
	if o.BaselineURL, err = absURL("RG_BASELINE_URL", baseline); err != nil {
		return o, poster{}, err
	}
	if o.CandidateURL, err = absURL("RG_CANDIDATE_URL", candidate); err != nil {
		return o, poster{}, err
	}

	ctxInfo, err := gate.GitHubContext(getenv, os.ReadFile)
	if err != nil {
		return o, poster{}, err
	}
	o.CommitSHA, o.PRNumber = ctxInfo.CommitSHA, ctxInfo.PRNumber

	if v := getenv("RG_SAMPLE_SIZE"); v != "" {
		if o.SampleSize, err = positiveInt("RG_SAMPLE_SIZE", v); err != nil {
			return o, poster{}, err
		}
	}
	if v := getenv("RG_CONCURRENCY"); v != "" {
		if o.Concurrency, err = positiveInt("RG_CONCURRENCY", v); err != nil {
			return o, poster{}, err
		}
	}
	if v := getenv("RG_NOISE_LEARNING"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return o, poster{}, fmt.Errorf("RG_NOISE_LEARNING must be true or false, got %q", v)
		}
		o.NoiseLearning = &b
	}
	if o.Timeout, err = duration(getenv, "RG_TIMEOUT"); err != nil {
		return o, poster{}, err
	}
	if o.RequestTimeout, err = duration(getenv, "RG_REQUEST_TIMEOUT"); err != nil {
		return o, poster{}, err
	}

	p := poster{projectID: projectID}
	if v := strings.TrimRight(getenv("RG_DASHBOARD_URL"), "/"); v != "" {
		if _, err := absURL("RG_DASHBOARD_URL", v); err != nil {
			return o, poster{}, err
		}
		p.dashboardURL = v
	}
	if wantComment(getenv("RG_COMMENT")) && o.PRNumber != nil && getenv("GITHUB_TOKEN") != "" && getenv("GITHUB_REPOSITORY") != "" {
		p.client = &github.Client{Token: getenv("GITHUB_TOKEN"), APIURL: getenv("GITHUB_API_URL")}
		p.repo = getenv("GITHUB_REPOSITORY")
	}
	return o, p, nil
}

// required reads variables that must be set and reports every missing one at
// once. It reads through getenv so tests can inject an environment.
func required(getenv func(string) string, names ...string) ([]string, error) {
	vals := make([]string, len(names))
	var missing []string
	for i, n := range names {
		if vals[i] = strings.TrimSpace(getenv(n)); vals[i] == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return vals, nil
}

func wantComment(v string) bool {
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	return err != nil || b
}

func absURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute http(s) URL, got %q", name, raw)
	}
	return u, nil
}

func positiveInt(name, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, v)
	}
	return n, nil
}

func duration(getenv func(string) string, name string) (time.Duration, error) {
	v := getenv(name)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a duration such as 90s or 15m, got %q", name, v)
	}
	return d, nil
}

// writeOutputs sets the Action's outputs. Every value is a status word, a
// number or a UUID that the API produced, so none can carry a newline.
func writeOutputs(getenv func(string) string, log *slog.Logger, res report.Result) {
	path := getenv("GITHUB_OUTPUT")
	if path == "" {
		return
	}
	appendFile(log, path, fmt.Sprintf("verdict=%s\nrun-id=%s\nbreaking-count=%d\nadditive-count=%d\n",
		res.Run.Status, res.Run.ID, res.Run.Breaking, res.Run.Additive))
}

func appendFile(log *slog.Logger, path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		log.Warn("could not write", "path", path, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		log.Warn("could not write", "path", path, "err", err)
	}
}
