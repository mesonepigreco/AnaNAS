#!/usr/bin/python3
"""Policy-only checks; never invokes mount, reads secrets or contacts the NAS."""
import copy
import unittest

import ananas_mount
from ananas_mount import direct_route, find_nas, parse_mount, valid_lan

INTERFACE = [{"ifname": "enp1s0", "operstate": "UP", "flags": ["UP", "LOWER_UP"],
              "addr_info": [{"family": "inet", "local": "10.23.42.17", "prefixlen": 24}]}]
CONNECTED = [{"dst": "10.23.42.0/24", "dev": "enp1s0", "scope": "link"}]
OPTIONS = "rw,nosuid,nodev,noexec,relatime - cifs //{0}/Nasdir rw,vers=3.1.1,seal,addr={0}"


def mountinfo(server):
    return f"36 25 0:50 / /mnt/nasdir {OPTIONS.format(server)}\n"


class LANMountPolicy(unittest.TestCase):
    def test_any_current_address_on_the_direct_physical_link(self):
        self.assertTrue(valid_lan(INTERFACE, CONNECTED))
        renewed = copy.deepcopy(INTERFACE)
        renewed[0]["addr_info"][0]["local"] = "10.23.42.21"
        self.assertTrue(valid_lan(renewed, CONNECTED))
        self.assertFalse(valid_lan(INTERFACE, []))
        self.assertFalse(valid_lan(INTERFACE + INTERFACE, CONNECTED))
        for flags in (["UP"], ["UP", "LOWER_UP", "POINTOPOINT"]):
            changed = copy.deepcopy(INTERFACE)
            changed[0]["flags"] = flags
            self.assertFalse(valid_lan(changed, CONNECTED))
        for field, value in (("prefixlen", 16), ("local", "10.23.43.21")):
            changed = copy.deepcopy(INTERFACE)
            changed[0]["addr_info"][0][field] = value
            self.assertFalse(valid_lan(changed, CONNECTED))

    def test_route_to_the_chosen_nas_stays_direct(self):
        route = [{"dev": "enp1s0", "prefsrc": "10.23.42.17"}]
        self.assertTrue(direct_route(INTERFACE, route))
        for field, value in [("gateway", "10.23.42.1"), ("dev", "tun0"), ("prefsrc", "10.23.42.18"), ("type", "unreachable")]:
            with self.subTest(field=field):
                changed = copy.deepcopy(route)
                changed[0][field] = value
                self.assertFalse(direct_route(INTERFACE, changed))
        self.assertFalse(direct_route(INTERFACE, route + route))

    def test_find_nas_prefers_known_addresses_and_needs_the_pin(self):
        probed = []

        def probe(address):
            probed.append(address)
            return address == "10.23.42.77"
        self.assertEqual(find_nas(["10.23.42.30", None], ["10.23.42.30", "10.23.42.50", "10.23.42.77"], probe), "10.23.42.77")
        self.assertEqual(probed[0], "10.23.42.30")
        self.assertEqual(probed.count("10.23.42.30"), 1)
        self.assertIsNone(find_nas(["10.23.42.30"], ["10.23.42.50"], lambda a: False))

    def test_mount_follows_the_share_address(self):
        self.assertIsNone(parse_mount(""))
        self.assertEqual(parse_mount(mountinfo("10.23.42.77")), "10.23.42.77")
        for bad in (mountinfo("10.23.43.77"), mountinfo("nas.local"),
                    mountinfo("10.23.42.77").replace("addr=10.23.42.77", "addr=10.23.42.30"),
                    mountinfo("10.23.42.77").replace(",seal", ""),
                    mountinfo("10.23.42.77").replace("/Nasdir", "/Other"),
                    mountinfo("10.23.42.77") * 2):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    parse_mount(bad)

    def test_pin_placeholder_is_a_sha256(self):
        self.assertRegex(ananas_mount.PIN, r"^[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
