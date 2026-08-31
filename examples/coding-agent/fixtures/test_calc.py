"""Trusted acceptance checks; standard library only, no package installation."""

import unittest
from calc import add, divide, mean


class CalculatorTests(unittest.TestCase):
    def test_add(self):
        self.assertEqual(add(2, 3), 5)
        self.assertEqual(add(-2, 2), 0)

    def test_divide(self):
        self.assertEqual(divide(10, 2), 5)
        with self.assertRaises(ValueError):
            divide(10, 0)

    def test_mean(self):
        self.assertEqual(mean([2, 4, 6]), 4)
        self.assertEqual(mean([9]), 9)


if __name__ == "__main__":
    unittest.main()
