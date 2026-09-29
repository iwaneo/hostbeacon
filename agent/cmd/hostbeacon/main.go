// Command hostbeacon is the Hostbeacon Agent.
package main

import (
	"fmt"

	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

func main() {
	fmt.Println(version.String())
}
