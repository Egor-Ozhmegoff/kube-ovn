"""Offline tests for the audit gate; these do not assert dataplane support."""

import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("openperouter_audit", REPO / "hack/check-openperouter.py")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


class AuditTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.source = Path(self.temp.name)
        (self.source / "api.go").write_bytes(b"reviewed bytes\n")
        self.evidence = {
            "commit": "pinned",
            "sha256": {"api.go": hashlib.sha256(b"reviewed bytes\n").hexdigest()},
            "blockers": [{"requirement": 12, "reason": "gateway MAC is not configurable"}],
        }

    def test_reviewed_source_reports_blocker_without_claiming_dataplane(self):
        result = AUDIT.audit(self.source, self.evidence)
        self.assertEqual(result["status"], "blocked")
        self.assertEqual(result["blockers"], self.evidence["blockers"])
        self.assertFalse(result["dataplaneTested"])

    def test_different_revision_requires_new_review(self):
        (self.source / "api.go").write_bytes(b"new API\n")
        with self.assertRaisesRegex(ValueError, "unreviewed source"):
            AUDIT.audit(self.source, self.evidence)

    def test_missing_source_does_not_pass(self):
        (self.source / "api.go").unlink()
        with self.assertRaises(FileNotFoundError):
            AUDIT.audit(self.source, self.evidence)

    def test_resolved_api_blockers_do_not_claim_dataplane_support(self):
        self.evidence["blockers"] = []
        self.evidence["integrationBlockers"] = ["native VRF recreate requires terminal defaults"]
        self.evidence["capabilities"] = ["gatewayMAC", "node routerMAC"]
        self.evidence["pendingValidation"] = ["local L3 egress", "L2 split horizon"]
        result = AUDIT.audit(self.source, self.evidence)
        self.assertEqual(result["status"], "api-reviewed")
        self.assertEqual(result["integrationBlockers"], self.evidence["integrationBlockers"])
        self.assertEqual(result["capabilities"], self.evidence["capabilities"])
        self.assertEqual(result["pendingValidation"], self.evidence["pendingValidation"])
        self.assertFalse(result["dataplaneTested"])


if __name__ == "__main__":
    unittest.main()
