//go:build linux

package sshd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	gssh "github.com/gliderlabs/ssh"

	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/workspace"
)

// serveCache carries a delegated share's cache: mounting its union, filling it,
// dropping what the client deleted, and handing back what the container wrote
// (ADR 0044).
//
// The channel closing is what releases a share, which is why there is no
// request for it: the session that asked for the mount is the only thing that
// needs it, and when it goes so does the mount.
//
// Runs as root, like serveNotify and for the same reason: it mounts inside a
// daemon's namespace and writes into a volume the account cannot reach. Every
// request is re-validated here rather than trusted, because this is a root
// process being told which paths to write and which to remove. See
// workspace.CacheRequest.Validate, which both sides call.
//
// One request per line, each answered before the next is read. Deliberately
// not pipelined: every op here changes a mount or a file, the client waits for
// each in turn anyway, and a protocol that could reorder them would have to
// explain what two overlapping applies to one share mean.
func (s *Server) serveCache(session gssh.Session, account sessionAccount) {
	if s.cfg.Unions == nil {
		_, _ = fmt.Fprintln(session.Stderr(), "workspace-cache: this workspace does not serve delegated shares")
		_ = session.Exit(1)
		return
	}

	// The greeting first and unconditionally, before anything is read. An
	// agent too old for this command runs it as a shell and exits 127 with no
	// output at all, so the client tells "too old" from "version mismatch" by
	// whether a greeting arrived (see core/tunnel.CacheCommand).
	enc := json.NewEncoder(session)
	if err := enc.Encode(workspace.CacheReply{Hello: &workspace.CacheHello{Version: workspace.CacheVersion}}); err != nil {
		_ = session.Exit(1)
		return
	}

	name := account.Name()
	defer s.cfg.Unions.ReleaseAccount(session.Context(), name)

	reader := bufio.NewReaderSize(session, workspace.MaxCacheFrame)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				s.log().Info("a cache session ended", "account", name, "err", err)
			}
			_ = session.Exit(0)
			return
		}

		var req workspace.CacheRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(workspace.CacheReply{Err: fmt.Sprintf("workspace-cache: %v", err)})
			continue
		}

		reply, payload := s.applyCache(session, account, req, reader)
		if err := enc.Encode(reply); err != nil {
			_ = session.Exit(1)
			return
		}
		// The payload follows its reply line, exactly as a request's does: the
		// client has been told how many bytes to read before they arrive.
		if len(payload) > 0 {
			if _, err := session.Write(payload); err != nil {
				_ = session.Exit(1)
				return
			}
		}
	}
}

// applyCache performs one request and says what happened.
//
// The payload of an apply is read whatever the outcome: it follows the frame on
// the same stream, so a request refused without draining it would leave a tar
// where the next line is expected and desynchronise everything after it.
func (s *Server) applyCache(session gssh.Session, account sessionAccount, req workspace.CacheRequest, body io.Reader) (workspace.CacheReply, []byte) {
	if err := req.Validate(); err != nil {
		if req.Op == workspace.OpApply {
			_, _ = io.CopyN(io.Discard, body, req.Bytes)
		}
		return workspace.CacheReply{Err: err.Error()}, nil
	}

	name := account.Name()
	ctx := session.Context()

	switch req.Op {
	case workspace.OpPrepare:
		target, err := s.cfg.Daemons.Ensure(ctx, name)
		if err != nil {
			return workspace.CacheReply{Err: fmt.Sprintf("workspace-cache: %v", err)}, nil
		}
		merged, err := s.cfg.Unions.Prepare(ctx, name, unions.Daemon{Host: target.Host, PID: target.PID}, req)
		if err != nil {
			return workspace.CacheReply{Err: err.Error()}, nil
		}
		return workspace.CacheReply{Merged: merged}, nil

	case workspace.OpApply:
		err := s.cfg.Unions.Apply(ctx, name, req.Export, io.LimitReader(body, req.Bytes))
		if err != nil {
			return workspace.CacheReply{Err: err.Error()}, nil
		}
		return workspace.CacheReply{}, nil

	case workspace.OpDrop:
		if err := s.cfg.Unions.Drop(ctx, name, req.Export, req.Paths); err != nil {
			return workspace.CacheReply{Err: err.Error()}, nil
		}
		return workspace.CacheReply{}, nil

	case workspace.OpChanges:
		changes, err := s.cfg.Unions.Changes(ctx, name, req.Export)
		if err != nil {
			return workspace.CacheReply{Err: err.Error()}, nil
		}
		return workspace.CacheReply{Changes: changes}, nil

	case workspace.OpPull:
		pulled, err := s.cfg.Unions.Pull(ctx, name, req.Export, req.Paths)
		if err != nil {
			return workspace.CacheReply{Err: err.Error()}, nil
		}
		return workspace.CacheReply{Bytes: int64(len(pulled))}, pulled

	}

	// Validate has already refused every op this agent does not know, so
	// reaching here would mean the two disagree.
	return workspace.CacheReply{Err: fmt.Sprintf("workspace-cache: nothing handles %q", req.Op)}, nil
}
