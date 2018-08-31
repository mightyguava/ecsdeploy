package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/fatih/color"
	"github.com/mightyguava/ecsdeploy/deployer"
	"github.com/sfreiberg/progress"
)

type reporter interface {
	Report(status *deployer.DeployStatus)
	Wait(ctx context.Context) error
}

type TerminalReporter struct {
	dots               int
	w                  io.Writer
	numLastReportLines int
	lastStage          deployer.Stage
}

func (r *TerminalReporter) Report(status *deployer.DeployStatus) {
	if r.w == nil {
		r.w = os.Stdout
	}

	if r.lastStage == deployer.StageWaitForDeploy {
		r.eraseLast(r.numLastReportLines)
		if status.Stage == deployer.StageWaitForDeploy {
			r.printMsg(status.Message)
		}
		r.numLastReportLines = r.printDeployStatus(status)
		r.dots++
		if status.Stage >= deployer.StageCompleted {
			fmt.Println()
		}
	}

	if status.Stage != r.lastStage {
		fmt.Println()
	}

	if status.Stage != deployer.StageWaitForDeploy {
		r.printMsg(status.Message)
	}

	r.lastStage = status.Stage

	if status.Done {
		fmt.Fprintln(r.w)
	}
}

func (r *TerminalReporter) printMsg(m *deployer.Message) {
	if m != nil {
		text := m.Text
		switch m.Type {
		case deployer.Info:
			color.White(text)
		case deployer.Success:
			color.Green(text)
		case deployer.Error:
			color.Red(text)
		}
	}
}

func (r *TerminalReporter) Wait(ctx context.Context) error {
	return nil
}

func (r *TerminalReporter) printDeployStatus(status *deployer.DeployStatus) int {
	lines := 1
	fmt.Fprintf(r.w, "current:  %v/%v running, %v pending\n", status.Current.Running, status.Current.Desired, status.Current.Pending)
	// If there were running tasks at the start of the deployment, always report the number of tasks stopped.
	if status.Previous.Total > 0 {
		lines++
		fmt.Fprintf(r.w, "previous: %v/%v stopped\n", status.Previous.Total-status.Previous.Running, status.Previous.Total)
	}
	for i := 0; i < r.dots; i++ {
		fmt.Fprint(r.w, ".")
	}
	return lines
}

const (
	seqEraseLine = "\033[K"
	seqUpOneLine = "\033[1A"
)

func (r *TerminalReporter) eraseLast(n int) {
	for i := 0; i < n; i++ {
		fmt.Print(seqUpOneLine)
		fmt.Print(seqEraseLine)
		fmt.Print("\r")
	}
}

type HTTPReporter struct {
	addr   string
	token  string
	sender *ThrottlingExecutor
}

func NewHTTPReporter(address, token string) (*HTTPReporter, error) {
	if _, err := url.Parse(address); err != nil {
		return nil, err
	}
	return &HTTPReporter{
		addr:   address,
		token:  token,
		sender: NewDiscardingSender(),
	}, nil
}

func (r *HTTPReporter) Report(status *deployer.DeployStatus) {
	r.sender.DoOrDiscard(func() error {
		return r.sendReport(status)
	})
}

func (r *HTTPReporter) Wait(ctx context.Context) error {
	return r.sender.Wait(ctx)
}

func (r *HTTPReporter) sendReport(status *deployer.DeployStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("error serializing deployments for HTTP reporting: %v", err)
	}
	req, err := http.NewRequest("POST", r.addr, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer: "+r.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error reporting by HTTP: %v", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("error response received: [%v] %v", resp.StatusCode, string(body))
	}
	return nil
}

type SlackReporter struct {
	sender  *ThrottlingExecutor
	token   string
	channel string
	prg     *progress.Progress
}

func NewSlackReporter(token, channel string) *SlackReporter {
	return &SlackReporter{
		sender:  NewDiscardingSender(),
		token:   token,
		channel: channel,
	}
}

func (r *SlackReporter) Report(status *deployer.DeployStatus) {
	r.sender.DoOrDiscard(func() error {
		return r.sendReport(status)
	})
}

func (r *SlackReporter) Wait(ctx context.Context) error {
	return r.sender.Wait(ctx)
}

func (r *SlackReporter) sendReport(s *deployer.DeployStatus) error {
	if r.prg == nil {
		r.prg = progress.New(r.token, r.channel, nil)
		r.prg.Opts.ShowEstTime = false
	}
	if s.Message != nil {
		return nil
	}
	r.prg.Opts.Task = fmt.Sprintf(`deploy %v to %v`, s.Service, s.Cluster)
	var percent int
	if s.Current.Desired+s.Previous.Total == 0 {
		percent = 100
	} else {
		percent = (s.Current.Running + s.Current.Pending/2 + (s.Previous.Total - s.Previous.Running)) * 100 / (s.Current.Desired + s.Previous.Total)
	}
	if percent == 0 {
		percent = 1
	}
	return r.prg.Update(percent)
}

type CompositeReporter []reporter

func (cr CompositeReporter) Report(status *deployer.DeployStatus) {
	for _, r := range cr {
		r.Report(status)
	}
}

func (cr CompositeReporter) Wait(ctx context.Context) error {
	for _, r := range cr {
		if err := r.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

type ThrottlingExecutor struct {
	done chan bool

	mu            *sync.Mutex
	fn            func() error
	dirty         bool
	reporting     bool
	waitingToStop bool
}

func NewDiscardingSender() *ThrottlingExecutor {
	return &ThrottlingExecutor{
		done: make(chan bool),
		mu:   &sync.Mutex{},
	}
}

// DoOrDiscard sends the deployment status via HTTP to the given address. This function returns immediately, and sends the
// HTTP requests in the background. If the HTTP endpoint responds slower than Report is called, only the latest deploy
// status is sent.
func (r *ThrottlingExecutor) DoOrDiscard(fn func() error) {
	r.mu.Lock()
	r.fn, r.dirty = fn, true
	r.mu.Unlock()
	go r.doReport()
}

func (r *ThrottlingExecutor) Wait(ctx context.Context) error {
	r.mu.Lock()
	r.waitingToStop = true
	if !r.dirty && !r.reporting {
		// Already done reporting, short circuit!
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return nil
	}
}

func (r *ThrottlingExecutor) doReport() {
	r.mu.Lock()
	// If another report routine is already running, or if there's nothing to report, just return
	if r.reporting || !r.dirty {
		r.mu.Unlock()
		return
	}
	// Claim that this routine is reporting so that no one else does, and clear the dirty bit
	r.reporting = true
	r.dirty = false
	fn := r.fn
	r.mu.Unlock()

	err := fn()
	if err != nil {
		color.Red(err.Error())

		r.mu.Lock()
		r.dirty = true // Try again
		r.mu.Unlock()
	}

	r.mu.Lock()
	r.reporting = false
	if r.dirty {
		// There's new information to report! Report again
		go r.doReport()
	} else if r.waitingToStop {
		// No new information to report, and we are waiting to stop, so stop
		close(r.done)
	}
	r.mu.Unlock()
}
