//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("router binary only runs on Linux")
}
