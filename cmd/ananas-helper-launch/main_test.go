package main

import (
	"testing"
	"time"
)

func TestLauncherRejectsBroaderIdentityAndNetworkScope(t *testing.T) {
	base := launchOptions{binary: "/private/helper", config: "/private/helper.json", listen: "10.23.42.30:8742", device: "eth0", prefix: "10.23.42.0/24", peer: "10.23.42.17", uid: 1000, gid: 100, write: true, lifetime: time.Minute}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	subnet := base
	subnet.peer = "10.23.42.0/24"
	if err := subnet.validate(); err != nil {
		t.Fatal("direct client subnet rejected:", err)
	}
	for _, mode := range []string{"root", "root group", "wildcard", "hostname", "public", "outside subnet", "wider subnet", "noncanonical subnet", "low port", "unbounded", "long test", "relative"} {
		t.Run(mode, func(t *testing.T) {
			o := base
			switch mode {
			case "root":
				o.uid = 0
			case "root group":
				o.gid = 0
			case "wildcard":
				o.listen = "0.0.0.0:8742"
			case "hostname":
				o.listen = "nas:8742"
			case "public":
				o.listen = "8.8.8.8:8742"
			case "outside subnet":
				o.peer = "192.168.2.17"
			case "wider subnet":
				o.peer = "10.23.0.0/16"
			case "noncanonical subnet":
				o.peer = "10.23.42.17/24"
			case "low port":
				o.listen = "10.23.42.30:22"
			case "unbounded":
				o.lifetime = 0
			case "long test":
				o.lifetime = time.Hour
			case "relative":
				o.binary = "helper"
			}
			if err := o.validate(); err == nil {
				t.Fatal("unsafe launcher scope accepted")
			}
		})
	}
}
