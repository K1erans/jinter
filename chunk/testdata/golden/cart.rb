require 'json'

module Shop
  # A shopping cart.
  class Cart
    attr_reader :items

    def initialize
      @items = []
    end

    def add(name, price, qty = 1)
      @items << { name: name, price: price, qty: qty }
      self
    end

    def total
      @items.sum { |i| i[:price] * i[:qty] }
    end

    def to_json(*args)
      { items: @items, total: total }.to_json(*args)
    end
  end

  def self.round2(x)
    (x * 100).round / 100.0
  end
end

cart = Shop::Cart.new
cart.add('tea', 3.5).add('cake', 2, 2)
puts cart.to_json
