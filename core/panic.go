// Turning a panic into an error, so that a mistake in the code ends only the work it happened in (`RunWithPanicAsError`).
package core

import (
	"fmt"
	"log"
	"runtime/debug"
)

// Runs `f` and returns its error.
// If `f` panics, the panic is stopped and returned as an error, and its stack goes to the server log.
//
// NOTE: `recover` stops a panic only in the goroutine that runs it, like a `catch` that sees only its own thread.
// A panic in a goroutine that the server starts ends the whole process, with every request it is serving.
// So every such goroutine must run its work through this function.
// A handler doesn't need to: `ServeAPIRequest` runs it through this function and answers 500.
// Run only the work, not code that holds a lock: a panic stopped while a lock is held leaves the lock held.
func RunWithPanicAsError(f func() error) (err error) {
	// A deferred function runs when `RunWithPanicAsError` returns, also during a panic.
	// `recover` there stops the panic, and setting the named result `err` changes what is returned
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("panic: %v\n%s", recovered, debug.Stack())
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return f()
}
