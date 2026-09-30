import unittest

from invoice import checkout, delivery, subtotal


class InvoiceTests(unittest.TestCase):
    def test_quantity(self):
        self.assertEqual(subtotal([(1200, 3), (700, 2)]), 5000)

    def test_empty_subtotal(self):
        self.assertEqual(subtotal([]), 0)

    def test_delivery_threshold(self):
        self.assertEqual(delivery(4999), 500)
        self.assertEqual(delivery(5000), 0)
        self.assertEqual(delivery(5001), 0)

    def test_checkout(self):
        self.assertEqual(checkout([(1200, 3), (700, 2)]), 5000)
        self.assertEqual(checkout([(1000, 2)]), 2500)


if __name__ == "__main__":
    unittest.main(verbosity=2)
