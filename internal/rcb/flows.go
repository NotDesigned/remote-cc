package rcb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// apiError is the workbench's error body.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Error is a failed workbench call; Status is 0 when the workbench was not
// reached at all.
type Error struct {
	Status int
	err    error
}

func (e *Error) Error() string {
	if e.err == nil {
		return fmt.Sprintf("remote-claude-browser answered HTTP %d", e.Status)
	}
	return e.err.Error()
}
func (e *Error) Unwrap() error { return e.err }

// Temporary reports whether a failed workbench call may work when tried
// again: the workbench was unreachable or had a server error.
func Temporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == 0 || e.Status >= 500)
}

// call does one request against the workbench; token "" sends none.
func call(base, token, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, &Error{0, fmt.Errorf("remote-claude-browser at %s: %w", base, err)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		var e apiError
		json.Unmarshal(b, &e)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && token != "":
			err = errors.New("remote-claude-browser no longer accepts this machine's connection; run: rcb login URL")
		case e.Message != "":
			err = errors.New(e.Message)
		default:
			err = fmt.Errorf("remote-claude-browser answered HTTP %d", resp.StatusCode)
		}
		return resp.StatusCode, &Error{resp.StatusCode, err}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("unexpected answer from %s (is this a remote-claude-browser with remote-cc support?)", base)
		}
	}
	return resp.StatusCode, nil
}

// Connect asks the workbench at base for a token of kind ("client" or
// "host") named name. show tells the user where to approve it; Connect then
// waits until they approve, deny, or the request expires.
func Connect(base, kind, name string, show func(code, link string)) (string, error) {
	var started struct {
		DeviceCode, UserCode, VerifyURL string
		Interval, ExpiresIn             int
	}
	if _, err := call(base, "", "POST", "/api/remote-cc/v1/devices", map[string]string{"kind": kind, "name": name}, &started); err != nil {
		return "", err
	}
	show(started.UserCode, started.VerifyURL)
	every := time.Duration(max(started.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(started.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(every)
		var v struct{ Status, Token string }
		code, err := call(base, "", "POST", "/api/remote-cc/v1/devices/token", map[string]string{"deviceCode": started.DeviceCode}, &v)
		if err != nil {
			if code == 0 { // network trouble: keep waiting
				continue
			}
			return "", err
		}
		if v.Token != "" {
			return v.Token, nil
		}
	}
	return "", errors.New("the connection request expired; run the command again")
}

// Grant is a short-lived permission for one session to run profile id
// through its node, redeemable only by an egress host.
func Grant(l Link, id string) (string, error) {
	var v struct{ Grant string }
	if _, err := call(l.URL, l.Token, "POST", "/api/remote-cc/v1/profiles/"+url.PathEscape(id)+"/grant", nil, &v); err != nil {
		return "", err
	}
	return v.Grant, nil
}

// Check has the workbench re-check the profile's exit through its node now.
func Check(l Link, id string) (Exit, error) {
	var v struct{ Exit Exit }
	_, err := call(l.URL, l.Token, "POST", "/api/remote-cc/v1/profiles/"+url.PathEscape(id)+"/check", nil, &v)
	return v.Exit, err
}

// StartLogin hands a Claude Code sign-in link to the workbench, which opens
// it in the profile's browser. status is "opened" if that browser is running,
// else "waiting" (it opens when the user opens the profile).
func StartLogin(l Link, id, link string) (loginID, status string, err error) {
	var v struct{ ID, Status string }
	_, err = call(l.URL, l.Token, "POST", "/api/remote-cc/v1/profiles/"+url.PathEscape(id)+"/logins", map[string]string{"url": link}, &v)
	return v.ID, v.Status, err
}

// ErrLoginGone means the workbench no longer knows the sign-in: the user
// cancelled it there, or it expired.
var ErrLoginGone = errors.New("the sign-in was cancelled or has expired in remote-claude-browser")

// PollLogin reports a sign-in's status and, once the user authorized it, the
// code Claude Code asks for (handed out once). A sign-in the workbench no
// longer knows is ErrLoginGone.
func PollLogin(l Link, loginID string) (status, code string, err error) {
	var v struct{ Status, Code string }
	st, err := call(l.URL, l.Token, "GET", "/api/remote-cc/v1/logins/"+url.PathEscape(loginID), nil, &v)
	if st == http.StatusNotFound || st == http.StatusGone {
		return "", "", ErrLoginGone
	}
	return v.Status, v.Code, err
}
