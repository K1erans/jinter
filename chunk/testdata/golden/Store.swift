import Foundation

/// Money in cents.
typealias Cents = Int

struct LineItem: Codable {
    let price: Cents
    let quantity: Int

    var total: Cents { price * quantity }
}

enum Status {
    case draft
    case paid(at: Date)
}

protocol Priced {
    func price() -> Cents
}

final class Store {
    private var orders: [String: [LineItem]] = [:]

    func add(_ item: LineItem, to id: String) {
        orders[id, default: []].append(item)
    }

    func total(_ id: String) -> Cents? {
        guard let items = orders[id] else {
            return nil
        }
        return items.reduce(0) { $0 + $1.total }
    }
}

extension Store: CustomStringConvertible {
    var description: String {
        "Store with \(orders.count) orders"
    }
}

func round2(_ x: Double) -> Double {
    (x * 100).rounded() / 100
}
