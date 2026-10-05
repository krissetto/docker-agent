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
	var disconnected bool
	var resources sync.WaitGroup
	var methods []js.Func
	object := js.Global().Get("Object").New()
	setMethod := func(name string, callback func(js.Value, []js.Value) any) {
		handle := js.FuncOf(func(this js.Value, args []js.Value) any {
			mu.Lock()
			if disconnected {
				mu.Unlock()
				return throwingError("authority disconnected")
			}
			resources.Add(1)
			mu.Unlock()
			defer resources.Done()
			return callback(this, args)
		})
		methods = append(methods, handle)
		object.Set("_"+name, handle)
	}
	setMethod("request", func(_ js.Value, args []js.Value) any {
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
			resources.Go(func() {
				defer rejectJSPanic(reject)
				data, err := client.request(clientCtx, method, path, body)
				if err != nil {
					reject(jsError(err))
					return
				}
				resolve(jsonJS(data))
			})
		})
	})
	setMethod("observe", func(_ js.Value, args []js.Value) any {
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
		js.Global().Call("eval", `(s => {s.close = () => {const close = s._close; delete s._close; if (close) close();};})`).Invoke(subscription)
		subscription.Set("done", newPromise(func(resolve, reject func(any)) {
			resources.Go(func() {
				defer rejectJSPanic(reject)
				defer func() {
					cancel()
					subscription.Delete("_close")
					releaseJSFunctions(closeFunc)
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
			})
		}))
		return subscription
	})
	setMethod("disconnect", func(_ js.Value, _ []js.Value) any {
		disconnect()
		mu.Lock()
		defer mu.Unlock()
		disconnected = true
		for _, cancel := range subscriptions {
			cancel()
		}
		go func() {
			resources.Wait()
			releaseJSFunctions(methods...)
		}()
		return nil
	})
	js.Global().Call("eval", `(a => {
		a.request = (...args) => a._request ? a._request(...args) : Promise.reject(new Error("authority disconnected"));
		a.observe = (...args) => {if (!a._observe) throw new Error("authority disconnected"); return a._observe(...args);};
		a.disconnect = () => {
			const disconnect = a._disconnect;
			delete a._request; delete a._observe; delete a._disconnect;
			if (disconnect) disconnect();
		};
	})`).Invoke(object)
	return object
}

func rejectJSPanic(reject func(any)) {
	if failure := recover(); failure != nil {
		reject(jsError(fmt.Errorf("JavaScript callback: %v", failure)))
	}
}

func releaseJSFunctions(handles ...js.Func) {
	var release js.Func
	release = js.FuncOf(func(js.Value, []js.Value) any {
		defer release.Release()
		for _, handle := range handles {
			handle.Release()
		}
		return nil
	})
	// Release only after active callbacks have returned to JavaScript.
	js.Global().Get("Promise").Call("resolve").Call("then", release)
}
