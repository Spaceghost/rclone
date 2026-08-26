// Build an rclone binary with the out-of-tree projection backend registered.
package main

import (
	_ "github.com/Spaceghost/rclone-projection-vfs/backend/projection"
	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"
	_ "github.com/rclone/rclone/lib/plugin"
)

func main() {
	cmd.Main()
}
