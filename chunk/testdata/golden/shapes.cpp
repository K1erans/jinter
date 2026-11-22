#include <cmath>
#include <memory>
#include <vector>

namespace shapes {

class Shape {
public:
    virtual ~Shape() = default;
    virtual double area() const = 0;
};

class Circle : public Shape {
public:
    explicit Circle(double r) : r_(r) {}
    double area() const override { return M_PI * r_ * r_; }

private:
    double r_;
};

template <typename T>
T sum(const std::vector<T>& xs) {
    T total{};
    for (const auto& x : xs) {
        total += x;
    }
    return total;
}

double total_area(const std::vector<std::unique_ptr<Shape>>& shapes) {
    double total = 0;
    for (const auto& s : shapes) {
        total += s->area();
    }
    return total;
}

}  // namespace shapes
