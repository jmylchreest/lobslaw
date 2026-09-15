import datetime as dt
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("prune", Path(__file__).with_name("prune-images.py"))
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)


class RetentionTests(unittest.TestCase):
    def test_release_tags_recent_uploads_and_latest_builds_survive(self):
        now = dt.datetime(2026, 9, 15, tzinfo=dt.timezone.utc)

        def version(i, tags, age=60):
            timestamp = (now - dt.timedelta(days=age)).isoformat()
            return dict(id=i, created_at=timestamp, updated_at=timestamp,
                        metadata={"container": {"tags": tags}})

        builds = [version(i, [f"sha-{i:07x}"], 40+i) for i in range(prune.KEEP_BUILDS+1)]
        versions = builds + [version(100, ["v0.1.0", "sha-abcdef0"]),
                             version(101, []), version(102, [], 1),
                             version(103, ["production"]),
                             version(104, list(prune.PINNED_TAGS))]
        self.assertEqual({v["id"] for v in prune.candidates(versions, now)},
                         {prune.KEEP_BUILDS, 101})

    def test_nested_image_and_attestation_manifests_remain_reachable(self):
        manifests = {"release": {"manifests": [{"digest": "index"}, {"digest": "attestation"}]},
                     "index": {"manifests": [{"digest": "image"}]},
                     "image": {}, "attestation": {}}
        self.assertEqual(prune.reachable(["release"], manifests.__getitem__), set(manifests))

    def test_inspection_failure_aborts_cleanup(self):
        with self.assertRaises(KeyError):
            prune.reachable(["missing"], {}.__getitem__)
