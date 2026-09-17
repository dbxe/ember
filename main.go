package main

import goruntime "runtime"

func main() {
	goruntime.LockOSThread()

	app := NewApp()
	app.Start()
	RunApp()
	app.Shutdown()
}
