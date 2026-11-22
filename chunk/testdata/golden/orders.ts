import { z } from "zod";
import type { Order, LineItem } from "./types";

// Money is kept in integer cents.
export type Cents = number;

export interface Discount {
  code: string;
  percent: number;
  appliesTo(item: LineItem): boolean;
}

export enum Status {
  Draft = "draft",
  Paid = "paid",
  Refunded = "refunded",
}

/** Rounds to two decimal places. */
export function round2(x: number): number {
  return Math.round(x * 100) / 100;
}

export class OrderService {
  private readonly orders = new Map<string, Order>();

  constructor(private readonly tax: number) {}

  /** Adds an order, replacing one with the same id. */
  add(order: Order): void {
    this.orders.set(order.id, order);
  }

  total(id: string, discounts: Discount[] = []): Cents {
    const order = this.orders.get(id);
    if (!order) {
      throw new Error(`no order ${id}`);
    }
    let sum = 0;
    for (const item of order.items) {
      let price = item.price * item.quantity;
      for (const d of discounts) {
        if (d.appliesTo(item)) {
          price -= (price * d.percent) / 100;
        }
      }
      sum += price;
    }
    return Math.round(sum * (1 + this.tax));
  }

  async refund(id: string): Promise<void> {
    const order = this.orders.get(id);
    if (!order || order.status !== Status.Paid) {
      return;
    }
    await fetch(`/api/orders/${id}/refund`, { method: "POST" });
    order.status = Status.Refunded;
  }

  get size(): number {
    return this.orders.size;
  }
}

export const schema = z.object({
  id: z.string(),
  items: z.array(z.object({ price: z.number(), quantity: z.number() })),
});

const retry = async <T>(fn: () => Promise<T>, attempts = 3): Promise<T> => {
  let last: unknown;
  for (let i = 0; i < attempts; i++) {
    try {
      return await fn();
    } catch (e) {
      last = e;
      await new Promise((r) => setTimeout(r, 2 ** i * 100));
    }
  }
  throw last;
};

export default retry;
