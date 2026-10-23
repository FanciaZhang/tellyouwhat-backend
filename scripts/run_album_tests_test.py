import unittest

from run_album_tests import groups


class StorageWorkerTests(unittest.TestCase):
    def test_every_case_runs_once_with_bounded_balanced_workers(self):
        names = [f'TestCase{i}' for i in range(19)]
        selected = groups(names)
        self.assertEqual(sorted(name for group in selected for name in group), sorted(names))
        self.assertEqual(len(selected), 4)
        self.assertLessEqual(max(map(len, selected)) - min(map(len, selected)), 1)
        self.assertEqual(groups(['TestOnly']), [['TestOnly']])
        for invalid in ([], ['TestSame', 'TestSame']):
            with self.assertRaises(ValueError):
                groups(invalid)


if __name__ == '__main__':
    unittest.main()
