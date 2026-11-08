package clusterops

import (
	"context"
	"io"
	"sync"
	"time"
)

type logBudget struct{ window, send, terminal time.Duration }

var productionLogBudget = logBudget{10 * time.Minute, 15 * time.Second, 5 * time.Second}

// StreamPodLogs supervises a single sender. Returning the handler closes the
// real gRPC stream and releases a Send waiting on HTTP/2 flow control; do not
// join that sender here. Both channels have strictly bounded capacity.
func (u *UseCase) StreamPodLogs(ctx context.Context, q PodLogQuery, send func(LogFrame) error) error {
	if err := u.cluster(ctx, q.ClusterID); err != nil {
		return err
	}
	if !validNamespace(q.Namespace) || !validSegment(q.PodName) || (q.Container != "" && !validSegment(q.Container)) || send == nil ||
		(q.TailLines != nil && *q.TailLines < 0) || (q.SinceSeconds != nil && *q.SinceSeconds <= 0) ||
		(q.LimitBytes != nil && *q.LimitBytes <= 0) || (q.SinceSeconds != nil && q.SinceTime != nil) {
		return NewError("INVALID_ARGUMENT")
	}
	select {
	case u.logSlots <- struct{}{}:
	default:
		return NewError("RESOURCE_EXHAUSTED")
	}
	defer func() { <-u.logSlots }()
	budget := u.logBudget
	start := time.Now()
	readCtx, cancelRead := context.WithDeadline(ctx, start.Add(budget.window))
	defer cancelRead()
	totalCtx, cancelTotal := context.WithDeadline(ctx, start.Add(budget.window+budget.terminal))
	defer cancelTotal()
	opened, err := u.kube.OpenPodLogs(readCtx, q)
	if ctx.Err() != nil {
		if opened.Reader != nil {
			_ = opened.Reader.Close()
		}
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if opened.Reader == nil {
		return NewError("INTERNAL_ERROR")
	}
	var once sync.Once
	closeReader := func() { once.Do(func() { _ = opened.Reader.Close() }) }
	defer closeReader()
	stopClose := context.AfterFunc(readCtx, closeReader)
	defer stopClose()
	frames := make(chan LogFrame)
	results := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case <-stop:
				return
			case f := <-frames:
				err := send(f)
				results <- err
				if err != nil {
					return
				}
			}
		}
	}()
	sendFrame := func(f LogFrame, wait time.Duration) error {
		frameCtx, cancel := context.WithTimeout(totalCtx, wait)
		defer cancel()
		timeout := func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return NewError("LOG_SEND_TIMEOUT")
		}
		if frameCtx.Err() != nil {
			return timeout()
		}
		select {
		case <-frameCtx.Done():
			return timeout()
		case frames <- f:
		}
		select {
		case <-frameCtx.Done():
			return timeout()
		case err := <-results:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	if err = sendFrame(LogFrame{Type: LogReady, PodUID: opened.PodUID, Container: opened.Container}, budget.send); err != nil {
		return err
	}
	type readResult struct {
		data []byte
		err  error
	}
	requests := make(chan struct{})
	reads := make(chan readResult)
	go func() {
		for {
			select {
			case <-readCtx.Done():
				return
			case <-requests:
			}
			data := make([]byte, 16*1024)
			n, err := opened.Reader.Read(data)
			select {
			case <-readCtx.Done():
				return
			case reads <- readResult{data: data[:n], err: err}:
			}
			if err != nil {
				return
			}
		}
	}()
	finish := func(reason string, readErr bool) error {
		cancelRead()
		closeReader()
		f := LogFrame{Type: LogEnd, Reason: reason}
		if readErr {
			f.Type = LogError
			f.Message = "Pod 日志读取失败"
		}
		if err := sendFrame(f, budget.terminal); err != nil {
			return err
		}
		if readErr {
			return NewError("LOG_READ_FAILED")
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readCtx.Err() != nil {
			return finish("STREAM_LIMIT_REACHED", false)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readCtx.Done():
			return finish("STREAM_LIMIT_REACHED", false)
		case requests <- struct{}{}:
		}
		var r readResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readCtx.Done():
			return finish("STREAM_LIMIT_REACHED", false)
		case r = <-reads:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readCtx.Err() != nil {
			return finish("STREAM_LIMIT_REACHED", false)
		}
		if len(r.data) > 0 {
			if err = sendFrame(LogFrame{Type: LogData, Data: r.data}, budget.send); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readCtx.Err() != nil {
			return finish("STREAM_LIMIT_REACHED", false)
		}
		if r.err == io.EOF {
			return finish("EOF", false)
		}
		if r.err != nil {
			return finish("LOG_READ_FAILED", true)
		}
	}
}
