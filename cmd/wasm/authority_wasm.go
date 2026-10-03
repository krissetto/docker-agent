//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"syscall/js"
)

func jsonJS(data []byte) js.Value { return js.Global().Get("JSON").Call("parse", string(data)) }

func connectAuthorityJS(_ js.Value, args []js.Value) any {
	if len(args) == 0 || args[0].Type() != js.TypeObject {
		return throwingError("connectAuthority: expected {url, token?}")
	}
	opts := args[0]
	token := ""
	if v := opts.Get("token"); v.Type() == js.TypeString {
		token = v.String()
	}
	client, err := newAuthorityClient(opts.Get("url").String(), token)
	if err != nil {
		return throwingError(err.Error())
	}
	clientCtx, disconnect := context.WithCancel(context.Background())
	var mu sync.Mutex
	subscriptions := map[uint64]context.CancelFunc{}
	var next uint64
	object := js.Global().Get("Object").New()
	object.Set("request", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 {
			return rejectedPromise("request: expected method and session-relative path")
		}
		method, path := args[0].String(), args[1].String()
		if path != "" && (!strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "..")) {
			return rejectedPromise("invalid session-relative path")
		}
		var body any
		if len(args) > 2 && !args[2].IsUndefined() {
			if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", args[2]).String()), &body); err != nil {
				return rejectedPromise(err.Error())
			}
		}
		return newPromise(func(resolve, reject func(any)) {
			go func() {
				data, err := client.request(clientCtx, method, path, body)
				if err != nil {
					reject(jsError(err))
					return
				}
				resolve(jsonJS(data))
			}()
		})
	}))
	object.Set("observe", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 || args[1].Type() != js.TypeFunction {
			return throwingError("observe: expected session ID, callback, and optional {since, epoch}")
		}
		id, callback := args[0].String(), args[1]
		var since *uint64
		epoch := ""
		if len(args) > 2 && args[2].Type() == js.TypeObject {
			if v := args[2].Get("since"); v.Type() == js.TypeNumber {
				n := v.Float()
				if n < 0 || n > 9007199254740991 || n != float64(uint64(n)) {
					return throwingError("since must be a non-negative safe integer")
				}
				cursor := uint64(n)
				since = &cursor
			}
			if v := args[2].Get("epoch"); v.Type() == js.TypeString {
				epoch = v.String()
			}
		}
		if epoch != "" && since == nil {
			return throwingError("epoch requires since")
		}
		ctx, cancel := context.WithCancel(clientCtx)
		mu.Lock()
		next++
		key := next
		subscriptions[key] = cancel
		mu.Unlock()
		subscription := js.Global().Get("Object").New()
		closeFunc := js.FuncOf(func(_ js.Value, _ []js.Value) any { cancel(); return nil })
		subscription.Set("_close", closeFunc)
		js.Global().Call("eval", `(s => {s.close = () => {if (s._close) s._close();};})`).Invoke(subscription)
		subscription.Set("done", newPromise(func(resolve, reject func(any)) {
			go func() {
				defer func() {
					cancel()
					subscription.Delete("_close")
					closeFunc.Release()
					mu.Lock()
					defer mu.Unlock()
					delete(subscriptions, key)
				}()
				err := client.observe(ctx, id, since, epoch, func(message authorityMessage) bool {
					data, err := json.Marshal(message)
					if err != nil {
						return false
					}
					callback.Invoke(jsonJS(data))
					return true
				})
				if ctx.Err() != nil {
					resolve(nil)
					return
				}
				if err != nil {
					reject(jsError(fmt.Errorf("observation disconnected: %w", err)))
					return
				}
				resolve(nil)
			}()
		}))
		return subscription
	}))
	object.Set("disconnect", js.FuncOf(func(_ js.Value, _ []js.Value) any {
		disconnect()
		mu.Lock()
		defer mu.Unlock()
		for _, cancel := range subscriptions {
			cancel()
		}
		return nil
	}))
	return object
}
