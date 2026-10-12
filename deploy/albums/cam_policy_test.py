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

    def test_pass_role_names_only_the_reviewed_service_role(self):
        policy = json.loads(Path(__file__).with_name("cam-media-role.policy.json").read_text())
        self.assertEqual(policy, {"version": "2.0", "statement": [{
            "effect": "allow", "action": ["cam:PassRole"],
            "resource": ["qcs::cam::uin/757772438:roleName/CI_QCSRole"],
        }]})


if __name__ == "__main__":
    unittest.main()
