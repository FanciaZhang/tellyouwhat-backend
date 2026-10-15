"""Static guard for these simple allow lists; this is not a CAM evaluator."""
import fnmatch
import json
from pathlib import Path
import unittest


class CIMediaPolicyTests(unittest.TestCase):
    def test_service_role_is_read_only_for_originals_and_scoped_to_outputs(self):
        policy = json.loads(Path(__file__).with_name("cam-ci-service.policy.json").read_text())
        self.assertEqual(policy["version"], "2.0")
        for statement in policy["statement"]:
            self.assertEqual(set(statement), {"effect", "action", "resource"})
            self.assertEqual(statement["effect"], "allow")
            self.assertTrue(all(action.startswith("cos:") and "*" not in action
                                for action in statement["action"]))

        def permits(action, resource):
            return any(action in entry["action"] and any(
                fnmatch.fnmatchcase(resource, pattern) for pattern in entry["resource"]
            ) for entry in policy["statement"])

        bucket = "qcs::cos:ap-guangzhou:uid/1251995834:tellyouwhat-albums-1251995834/"
        original = bucket + "albums/originals/owner/upload/video"
        output = bucket + "albums/video-previews/owner/job/daily"
        for action, resource, expected in [
            ("cos:GetObject", original, True),
            ("cos:HeadObject", original, True),
            ("cos:PutObject", output, True),
            ("cos:UploadPart", output, True),
            ("cos:UploadPart", original, False),
            ("cos:CompleteMultipartUpload", output, True),
            ("cos:AbortMultipartUpload", output, True),
            ("cos:PutObject", original, False),
            ("cos:DeleteObject", original, False),
            ("cos:DeleteObjectVersion", original, False),
            ("cos:DeleteObjectVersion", output, False),
            ("cos:GetObject", bucket + "albums/staging/source", False),
            ("cos:PutObject", bucket + "albums/video-previews-other/output", False),
            ("cos:GetObject", original.replace("tellyouwhat-albums-", "other-"), False),
            ("cos:PutObject", output.replace("tellyouwhat-albums-", "other-"), False),
            ("cos:PutBucketACL", bucket + "*", False),
            ("cos:ListBucket", bucket + "*", False),
            ("sts:AssumeRole", "*", False),
        ]:
            with self.subTest(action=action, resource=resource):
                self.assertEqual(permits(action, resource), expected)

    def test_backup_identity_has_only_dedicated_recovery_access(self):
        policy = json.loads(Path(__file__).with_name("cam-test-backup.policy.json").read_text())
        base = "qcs::cos:ap-guangzhou:uid/1251995834:tellyouwhat-albums-backups-1251995834/"
        self.assertEqual(policy, {"version": "2.0", "statement": [
            {"effect": "allow", "action": ["cos:GetBucketVersioning"], "resource": [base + "*"]},
            {"effect": "allow", "action": ["cos:PutObject", "cos:GetObject"], "resource": [base + "albums/recovery/*"]},
        ]})

    def test_pass_role_names_only_the_reviewed_service_role(self):
        policy = json.loads(Path(__file__).with_name("cam-media-role.policy.json").read_text())
        self.assertEqual(policy, {"version": "2.0", "statement": [{
            "effect": "allow", "action": ["cam:PassRole"],
            "resource": ["qcs::cam::uin/757772438:roleName/CI_QCSRole"],
        }]})

    def test_backend_upload_part_is_limited_to_video_previews(self):
        policy = json.loads(Path(__file__).with_name("cam-backend.policy.json").read_text())
        matches = [entry for entry in policy["statement"] if "cos:UploadPart" in entry["action"]]
        self.assertEqual(matches, [{"effect": "allow", "action": ["cos:UploadPart"],
            "resource": ["qcs::cos:ap-guangzhou:uid/1251995834:tellyouwhat-albums-1251995834/albums/video-previews/*"]}])


    def test_backend_version_inventory_requires_a_media_prefix(self):
        policy = json.loads(Path(__file__).with_name("cam-backend.policy.json").read_text())
        entries = [entry for entry in policy["statement"] if "cos:GetBucketObjectVersions" in entry["action"]]
        self.assertEqual(len(entries), 1)
        entry = entries[0]
        self.assertEqual(set(entry), {"effect", "action", "resource", "condition"})
        self.assertEqual(entry["effect"], "allow")
        self.assertEqual(entry["action"], ["cos:GetBucketObjectVersions"])
        self.assertEqual(entry["resource"], ["qcs::cos:ap-guangzhou:uid/1251995834:tellyouwhat-albums-1251995834/*"])
        patterns = ["albums/" + area + "/*" for area in (
            "staging", "originals", "derivatives", "video-previews", "live-pairs")]
        self.assertEqual(entry["condition"], {"string_like": {"cos:prefix": patterns}})
        for prefix, expected in [
            ("", False), ("albums/", False), ("health/", False),
            ("albums/originals-other/", False), ("albums/Originals/", False),
            ("albums/staging/live-pairs/owner/", True),
            ("albums/originals/owner/", True),
        ]:
            with self.subTest(prefix=prefix):
                self.assertEqual(any(fnmatch.fnmatchcase(prefix, p) for p in patterns), expected)
        for entry in policy["statement"]:
            self.assertFalse(any("Delete" in action or "*" in action for action in entry["action"]))


if __name__ == "__main__":
    unittest.main()
