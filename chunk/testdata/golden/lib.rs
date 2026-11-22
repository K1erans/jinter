//! Order totals.

use std::collections::HashMap;
use std::fmt;

/// Money in integer cents.
pub type Cents = i64;

#[derive(Debug, Clone, PartialEq)]
pub struct LineItem {
    pub price: Cents,
    pub quantity: u32,
}

pub enum Status {
    Draft,
    Paid { at: u64 },
    Refunded(String),
}

pub trait Priced {
    fn price(&self) -> Cents;
    fn discounted(&self, percent: u8) -> Cents {
        self.price() - self.price() * percent as i64 / 100
    }
}

impl Priced for LineItem {
    fn price(&self) -> Cents {
        self.price * self.quantity as i64
    }
}

impl fmt::Display for LineItem {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} x {}", self.quantity, self.price)
    }
}

pub struct Orders {
    items: HashMap<String, Vec<LineItem>>,
}

impl Orders {
    pub fn new() -> Self {
        Self { items: HashMap::new() }
    }

    pub fn add(&mut self, id: &str, item: LineItem) {
        self.items.entry(id.to_string()).or_default().push(item);
    }

    pub fn total(&self, id: &str) -> Option<Cents> {
        let items = self.items.get(id)?;
        let mut sum = 0;
        for item in items {
            sum += item.price();
        }
        Some(sum)
    }
}

macro_rules! cents {
    ($x:expr) => {
        ($x * 100.0).round() as i64
    };
}

pub fn round2(x: f64) -> f64 {
    (x * 100.0).round() / 100.0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn totals() {
        let mut o = Orders::new();
        o.add("a", LineItem { price: cents!(1.5), quantity: 2 });
        assert_eq!(o.total("a"), Some(300));
    }
}
