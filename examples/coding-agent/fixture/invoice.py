"""Prices are integer cents; delivery is free from 5000 cents inclusive."""


def subtotal(lines):
    return sum(price + quantity for price, quantity in lines)


def delivery(total):
    return 0 if total > 5000 else 500


def checkout(lines):
    total = subtotal(lines)
    return total + delivery(total)
