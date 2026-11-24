//go:build !testhooks

package deploy

// hooksCompiled is false in every build except one made with -tags testhooks,
// which only internal/testbin does. The default (shipped) build has no pause
// points: pausePoint is an empty function the compiler removes.
const hooksCompiled = false

// pausePoint marks a stage boundary of a deploy (name is "<stage>:<before|after>").
// In the default build it does nothing.
func pausePoint(string) {}
