#!/usr/bin/python3
"""Policy-only checks; never invokes mount, reads secrets or contacts the NAS."""
import copy
import unittest

from ananas_mount import valid_lan


class LANMountPolicy(unittest.TestCase):
    def test_only_the_configured_direct_physical_address(self):
        interface = [{"ifname": "enp1s0", "operstate": "UP", "flags": ["UP", "LOWER_UP"],
                      "addr_info": [{"family": "inet", "local": "10.23.42.17", "prefixlen": 24}]}]
        route = [{"dev": "enp1s0", "prefsrc": "10.23.42.17"}]
        connected = [{"dst": "10.23.42.0/24", "dev": "enp1s0", "scope": "link"}]
        self.assertTrue(valid_lan(interface, route, connected))
        for label, field, value in [("gateway", "gateway", "10.23.42.1"), ("VPN", "dev", "tun0"),
                                     ("source changed", "prefsrc", "10.23.42.18"), ("route denied", "type", "unreachable")]:
            with self.subTest(label=label):
                changed = copy.deepcopy(route)
                changed[0][field] = value
                self.assertFalse(valid_lan(interface, changed, connected))
        self.assertFalse(valid_lan(interface, route, []))
        self.assertFalse(valid_lan(interface, route + route, connected))
        for flags in (["UP"], ["UP", "LOWER_UP", "POINTOPOINT"]):
            changed = copy.deepcopy(interface)
            changed[0]["flags"] = flags
            self.assertFalse(valid_lan(changed, route, connected))
        changed = copy.deepcopy(interface)
        changed[0]["addr_info"][0]["prefixlen"] = 16
        self.assertFalse(valid_lan(changed, route, connected))


if __name__ == "__main__":
    unittest.main()
