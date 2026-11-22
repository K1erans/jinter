"""Billing helpers."""

import math
from dataclasses import dataclass, field
from typing import Iterable


@dataclass
class LineItem:
    price: float
    quantity: int = 1
    tags: list[str] = field(default_factory=list)

    def total(self) -> float:
        return self.price * self.quantity


def round2(x: float) -> float:
    """Rounds to two decimal places."""
    return math.floor(x * 100 + 0.5) / 100


class Invoice:
    """An invoice with line items and a tax rate."""

    def __init__(self, items: Iterable[LineItem], tax: float = 0.2):
        self.items = list(items)
        self.tax = tax

    def subtotal(self) -> float:
        return sum(i.total() for i in self.items)

    def total(self) -> float:
        sub = self.subtotal()
        if sub <= 0:
            return 0.0
        return round2(sub * (1 + self.tax))

    @property
    def tagged(self) -> dict[str, list[LineItem]]:
        out: dict[str, list[LineItem]] = {}
        for item in self.items:
            for tag in item.tags:
                out.setdefault(tag, []).append(item)
        return out

    @staticmethod
    def parse(rows):
        items = []
        for row in rows:
            price, qty = row.split(",")
            items.append(LineItem(float(price), int(qty)))
        return Invoice(items)


def retry(fn, attempts=3):
    last = None
    for i in range(attempts):
        try:
            return fn()
        except Exception as e:  # noqa: BLE001
            last = e
    raise last


if __name__ == "__main__":
    print(Invoice.parse(["1.5,2", "3,1"]).total())
