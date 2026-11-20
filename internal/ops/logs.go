// Package ops does the work behind tug's daily commands: logs, exec and
// debug containers, port-forward, rollouts, and explaining a workload: why
// it is not ready and what it is made of. It has no presentation, so the CLI
// and a later TUI share it.
package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// maxLogLine is the longest log line read before it is split.
const maxLogLine = 1 << 20

// LogOptions select what to stream.
type LogOptions struct {
	// Container limits the stream to one container; "" means all of them.
	Container  string
	Follow     bool
	Previous   bool
	Since      time.Duration
	Tail       int64 // lines from the end; negative means all
	Timestamps bool
}

// LogLine is one line from one container.
type LogLine struct {
	Pod       string
	Container string
	Text      string
}

// LogSink receives lines and per-stream errors. Logs calls it from one
// goroutine at a time.
type LogSink struct {
	Line  func(LogLine)
	Error func(pod, container string, err error)
}

// Logs streams logs from every pod of a target. When following a workload, it
// also watches for pods that join its selector and streams them as they start.
func Logs(ctx context.Context, cs kubernetes.Interface, t resolve.Target, opts LogOptions, sink LogSink) error {
	pods, err := resolve.Pods(ctx, cs, t)
	if err != nil {
		return err
	}
	if len(pods) == 0 && (!opts.Follow || !t.IsWorkload()) {
		return fmt.Errorf("%s has no pods", t)
	}

	var mu sync.Mutex
	locked := LogSink{
		Line: func(l LogLine) {
			mu.Lock()
			defer mu.Unlock()
			sink.Line(l)
		},
		Error: func(pod, container string, err error) {
			if sink.Error == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			sink.Error(pod, container, err)
		},
	}

	s := &streamer{cs: cs, ns: t.Namespace, opts: opts, sink: locked, started: map[string]bool{}}
	for i := range pods {
		s.startPod(ctx, &pods[i])
	}
	if opts.Follow && t.IsWorkload() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.watchNewPods(ctx, t)
		}()
	}
	s.wg.Wait()
	if s.streams == 0 && len(pods) > 0 {
		return fmt.Errorf("no container in %s matched %q", t, opts.Container)
	}
	return nil
}

type streamer struct {
	cs   kubernetes.Interface
	ns   string
	opts LogOptions
	sink LogSink

	wg      sync.WaitGroup
	mu      sync.Mutex
	started map[string]bool
	streams int
}

// containers returns the containers of a pod to stream: every app
// container, or the one asked for, which may also be an init or ephemeral
// container.
func (s *streamer) containers(p *corev1.Pod) []string {
	var names []string
	for _, c := range p.Spec.Containers {
		names = append(names, c.Name)
	}
	if s.opts.Container == "" {
		return names
	}
	for _, c := range p.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range p.Spec.EphemeralContainers {
		names = append(names, c.Name)
	}
	if slices.Contains(names, s.opts.Container) {
		return []string{s.opts.Container}
	}
	return nil
}

// startPod starts one stream per selected container that has not started yet.
func (s *streamer) startPod(ctx context.Context, p *corev1.Pod) {
	for _, name := range s.containers(p) {
		key := p.Name + "/" + name
		s.mu.Lock()
		if s.started[key] {
			s.mu.Unlock()
			continue
		}
		s.started[key] = true
		s.streams++
		s.mu.Unlock()

		s.wg.Add(1)
		go func(pod, container string) {
			defer s.wg.Done()
			if err := s.stream(ctx, pod, container); err != nil && !errors.Is(err, context.Canceled) {
				s.sink.Error(pod, container, err)
			}
		}(p.Name, name)
	}
}

func (s *streamer) stream(ctx context.Context, pod, container string) error {
	po := &corev1.PodLogOptions{
		Container:  container,
		Follow:     s.opts.Follow,
		Previous:   s.opts.Previous,
		Timestamps: s.opts.Timestamps,
	}
	if s.opts.Tail >= 0 {
		po.TailLines = &s.opts.Tail
	}
	if s.opts.Since > 0 {
		secs := int64(s.opts.Since.Seconds())
		po.SinceSeconds = &secs
	}
	rc, err := s.cs.CoreV1().Pods(s.ns).GetLogs(pod, po).Stream(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	return scanLines(rc, func(text string) {
		s.sink.Line(LogLine{Pod: pod, Container: container, Text: text})
	})
}

func scanLines(r io.Reader, fn func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLogLine)
	for sc.Scan() {
		fn(sc.Text())
	}
	return sc.Err()
}

// watchNewPods starts streams for pods that start running after Logs began.
func (s *streamer) watchNewPods(ctx context.Context, t resolve.Target) {
	w, err := s.cs.CoreV1().Pods(t.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: t.Selector.String()})
	if err != nil {
		s.sink.Error("", "", fmt.Errorf("watching for new pods: %w", err))
		return
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			if ev.Type != watch.Added && ev.Type != watch.Modified {
				continue
			}
			if p, ok := ev.Object.(*corev1.Pod); ok && p.Status.Phase == corev1.PodRunning {
				s.startPod(ctx, p)
			}
		}
	}
}
