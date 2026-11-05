//go:build !darwin

package runnerproc

func scanner() processScanner { return procfsScanner() }
