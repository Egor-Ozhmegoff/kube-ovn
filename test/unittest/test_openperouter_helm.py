"""Render contracts for actual agent tunnel flags; no Kubernetes connection."""

from pathlib import Path
import shutil
import subprocess
import unittest

import yaml


REPO = Path(__file__).resolve().parents[2]
HELM = shutil.which("helm")


@unittest.skipUnless(HELM, "helm is required for render contracts")
class OpenPERouterHelmTest(unittest.TestCase):
    def render(self, chart, *args):
        return subprocess.run(
            [HELM, "template", "lab", str(REPO / "charts" / chart), *args],
            check=False, text=True, capture_output=True,
        )

    def test_actual_daemon_tunnel_matches_explicit_vxlan_contract(self):
        for chart, fixture in [("kube-ovn", "helm-v1.yaml"), ("kube-ovn-v2", "helm-v2.yaml")]:
            with self.subTest(chart=chart):
                result = self.render(chart, "-f", str(REPO / "docs/openperouter/examples" / fixture))
                self.assertEqual(result.returncode, 0, result.stderr)
                enabled = {}
                for obj in yaml.safe_load_all(result.stdout):
                    if not obj or obj["kind"] not in ("Deployment", "DaemonSet"):
                        continue
                    for container in obj["spec"]["template"]["spec"]["containers"]:
                        if "--enable-openperouter=true" in container.get("args", []):
                            enabled[obj["kind"]] = container
                self.assertEqual(set(enabled), {"Deployment", "DaemonSet"})
                self.assertIn("--network-type=vxlan", enabled["DaemonSet"]["args"])
                self.assertIn("--network-type=geneve", enabled["Deployment"]["args"])
                self.assertTrue(any(m.get("readOnly") for m in enabled["DaemonSet"]["volumeMounts"]
                                    if m["name"] == "openperouter-bootstrap"))

    def test_geneve_default_and_vlan_cannot_enable_integration(self):
        for chart, network, tunnel in [
            ("kube-ovn", "networking.NETWORK_TYPE", "networking.TUNNEL_TYPE"),
            ("kube-ovn-v2", "networking.networkType", "networking.tunnelType"),
        ]:
            for args in [[], ["--set", network + "=vlan", "--set", tunnel + "=vxlan"]]:
                with self.subTest(chart=chart, args=args):
                    result = self.render(chart, "--set", "openperouter.enabled=true", *args)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("OpenPERouter", result.stderr)

    def test_default_render_keeps_integration_disabled(self):
        for chart in ["kube-ovn", "kube-ovn-v2"]:
            with self.subTest(chart=chart):
                result = self.render(chart)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn("--enable-openperouter=true", result.stdout)
                self.assertNotIn("name: openperouter-bootstrap", result.stdout)


if __name__ == "__main__":
    unittest.main()
