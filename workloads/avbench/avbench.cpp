// avbench: deterministic, autonomy-style CPU workloads for benchgrid.
//
// Each profile is one kernel of the kind an autonomy stack runs every cycle,
// at one of three sizes. Inputs come from a seeded generator, so the same
// profile and seed produce the same output on the same platform, and the
// program prints a checksum of that output for the agent to compare across
// iterations. A benchmark whose output changes between iterations is not
// measuring one thing, and the agent fails the run.
//
// Only the kernel is timed. Input generation happens before the clock starts,
// and the time is reported as work_ns alongside the agent's own wall clock
// measurement of the whole process.
//
// --slowdown P adds CPU work equal to P percent of the kernel's own measured
// time, without changing its output, which is how regressions of a known size
// are injected for the CI gate's evaluation. It is proportional rather than a
// number of extra repetitions so that a 3% regression is 3% on every profile,
// not rounded to zero on the ones with few repetitions.

#include <algorithm>
#include <array>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <limits>
#include <map>
#include <queue>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>

namespace {

// splitmix64: small, fast, and identical on every platform.
struct Rng {
  uint64_t s;
  explicit Rng(uint64_t seed) : s(seed) {}
  uint64_t next() {
    uint64_t z = (s += 0x9e3779b97f4a7c15ULL);
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
    return z ^ (z >> 31);
  }
  double uniform() { return (next() >> 11) * (1.0 / 9007199254740992.0); }
  double range(double lo, double hi) { return lo + (hi - lo) * uniform(); }
};

// The checksum hashes values rounded to a fixed precision, so it is stable
// against nothing but genuine changes in the computed result.
struct Checksum {
  uint64_t h = 1469598103934665603ULL;
  void add(uint64_t v) {
    for (int i = 0; i < 8; i++) {
      h ^= (v >> (8 * i)) & 0xff;
      h *= 1099511628211ULL;
    }
  }
  void add(double v) { add(static_cast<uint64_t>(static_cast<int64_t>(std::llround(v * 1e6)))); }
};

struct P3 {
  double x, y, z;
};

std::vector<P3> cloud(Rng& r, size_t n) {
  std::vector<P3> pts(n);
  for (auto& p : pts) {
    // A ground plane with clutter above it, roughly what one lidar sweep of a
    // street looks like to the kernels below.
    if (r.uniform() < 0.6) {
      p = {r.range(-40, 40), r.range(-40, 40), r.range(-0.05, 0.05)};
    } else {
      p = {r.range(-40, 40), r.range(-40, 40), r.range(0.2, 3.0)};
    }
  }
  return pts;
}

// ---- voxel_downsample ------------------------------------------------------

void voxel_downsample(const std::vector<P3>& pts, double leaf, Checksum& c) {
  struct Acc {
    double x = 0, y = 0, z = 0;
    uint32_t n = 0;
  };
  std::unordered_map<uint64_t, Acc> grid;
  grid.reserve(pts.size());
  for (const auto& p : pts) {
    auto k = [&](double v) { return static_cast<uint64_t>(static_cast<int64_t>(std::floor(v / leaf)) + (1 << 20)); };
    uint64_t key = (k(p.x) << 42) | (k(p.y) << 21) | k(p.z);
    Acc& a = grid[key];
    a.x += p.x, a.y += p.y, a.z += p.z, a.n++;
  }
  double sx = 0, sy = 0, sz = 0;
  for (const auto& [key, a] : grid) {
    sx += a.x / a.n, sy += a.y / a.n, sz += a.z / a.n;
  }
  c.add(static_cast<uint64_t>(grid.size()));
  c.add(sx), c.add(sy), c.add(sz);
}

// ---- kd tree, used by knn and icp ------------------------------------------

struct KdTree {
  std::vector<P3> pts;
  std::vector<int> idx;
  explicit KdTree(std::vector<P3> p) : pts(std::move(p)), idx(pts.size()) {
    for (size_t i = 0; i < idx.size(); i++) idx[i] = static_cast<int>(i);
    build(0, static_cast<int>(idx.size()), 0);
  }
  static double axis(const P3& p, int a) { return a == 0 ? p.x : a == 1 ? p.y : p.z; }
  void build(int lo, int hi, int depth) {
    if (hi - lo <= 1) return;
    int mid = (lo + hi) / 2, a = depth % 3;
    std::nth_element(idx.begin() + lo, idx.begin() + mid, idx.begin() + hi,
                     [&](int i, int j) { return axis(pts[i], a) < axis(pts[j], a); });
    build(lo, mid, depth + 1);
    build(mid + 1, hi, depth + 1);
  }
  void knn(const P3& q, int k, int lo, int hi, int depth, std::priority_queue<std::pair<double, int>>& best) const {
    if (lo >= hi) return;
    int mid = (lo + hi) / 2, a = depth % 3;
    const P3& p = pts[idx[mid]];
    double dx = p.x - q.x, dy = p.y - q.y, dz = p.z - q.z, d = dx * dx + dy * dy + dz * dz;
    if (static_cast<int>(best.size()) < k) {
      best.push({d, idx[mid]});
    } else if (d < best.top().first) {
      best.pop();
      best.push({d, idx[mid]});
    }
    double diff = axis(q, a) - axis(p, a);
    int nlo = diff < 0 ? lo : mid + 1, nhi = diff < 0 ? mid : hi;
    int flo = diff < 0 ? mid + 1 : lo, fhi = diff < 0 ? hi : mid;
    knn(q, k, nlo, nhi, depth + 1, best);
    if (static_cast<int>(best.size()) < k || diff * diff < best.top().first) knn(q, k, flo, fhi, depth + 1, best);
  }
  std::vector<std::pair<double, int>> query(const P3& q, int k) const {
    std::priority_queue<std::pair<double, int>> best;
    knn(q, k, 0, static_cast<int>(idx.size()), 0, best);
    std::vector<std::pair<double, int>> out;
    while (!best.empty()) out.push_back(best.top()), best.pop();
    return out;
  }
};

void knn_search(const std::vector<P3>& pts, const std::vector<P3>& queries, int k, Checksum& c) {
  KdTree tree(pts);
  double acc = 0;
  for (const auto& q : queries) {
    for (const auto& [d, i] : tree.query(q, k)) acc += std::sqrt(d) + i * 1e-9;
  }
  c.add(acc);
}

// ---- icp_step --------------------------------------------------------------

// One point-to-point ICP iteration: correspondences by nearest neighbour,
// then the best rotation from the 3x3 cross covariance by Jacobi SVD.
void icp_step(const std::vector<P3>& target, const std::vector<P3>& source, Checksum& c) {
  KdTree tree(target);
  P3 mt{0, 0, 0}, ms{0, 0, 0};
  std::vector<P3> matched(source.size());
  for (size_t i = 0; i < source.size(); i++) {
    matched[i] = tree.pts[tree.query(source[i], 1)[0].second];
    mt.x += matched[i].x, mt.y += matched[i].y, mt.z += matched[i].z;
    ms.x += source[i].x, ms.y += source[i].y, ms.z += source[i].z;
  }
  double n = static_cast<double>(source.size());
  mt = {mt.x / n, mt.y / n, mt.z / n};
  ms = {ms.x / n, ms.y / n, ms.z / n};
  double H[3][3] = {};
  for (size_t i = 0; i < source.size(); i++) {
    double s[3] = {source[i].x - ms.x, source[i].y - ms.y, source[i].z - ms.z};
    double t[3] = {matched[i].x - mt.x, matched[i].y - mt.y, matched[i].z - mt.z};
    for (int r = 0; r < 3; r++)
      for (int col = 0; col < 3; col++) H[r][col] += s[r] * t[col];
  }
  // Eigen decomposition of H^T H by cyclic Jacobi gives the singular values,
  // which is all the checksum needs and all the cost model cares about.
  double A[3][3];
  for (int i = 0; i < 3; i++)
    for (int j = 0; j < 3; j++) {
      A[i][j] = 0;
      for (int k = 0; k < 3; k++) A[i][j] += H[k][i] * H[k][j];
    }
  for (int sweep = 0; sweep < 32; sweep++) {
    for (int p = 0; p < 2; p++)
      for (int q = p + 1; q < 3; q++) {
        if (std::fabs(A[p][q]) < 1e-12) continue;
        double th = 0.5 * std::atan2(2 * A[p][q], A[q][q] - A[p][p]);
        double cs = std::cos(th), sn = std::sin(th);
        for (int k = 0; k < 3; k++) {
          double akp = A[k][p], akq = A[k][q];
          A[k][p] = cs * akp - sn * akq;
          A[k][q] = sn * akp + cs * akq;
        }
        for (int k = 0; k < 3; k++) {
          double apk = A[p][k], aqk = A[q][k];
          A[p][k] = cs * apk - sn * aqk;
          A[q][k] = sn * apk + cs * aqk;
        }
      }
  }
  std::array<double, 3> sv = {std::sqrt(std::fabs(A[0][0])), std::sqrt(std::fabs(A[1][1])), std::sqrt(std::fabs(A[2][2]))};
  std::sort(sv.begin(), sv.end());
  for (double v : sv) c.add(v);
  c.add(mt.x - ms.x), c.add(mt.y - ms.y), c.add(mt.z - ms.z);
}

// ---- conv2d ----------------------------------------------------------------

void conv2d(const std::vector<float>& img, int w, int h, int ksize, Checksum& c) {
  std::vector<float> kernel(ksize * ksize);
  float ksum = 0;
  int r = ksize / 2;
  for (int y = 0; y < ksize; y++)
    for (int x = 0; x < ksize; x++) {
      float v = std::exp(-((x - r) * (x - r) + (y - r) * (y - r)) / (2.0f * r * r + 1));
      kernel[y * ksize + x] = v, ksum += v;
    }
  for (auto& v : kernel) v /= ksum;
  std::vector<float> out(img.size());
  for (int y = r; y < h - r; y++)
    for (int x = r; x < w - r; x++) {
      float acc = 0;
      for (int ky = -r; ky <= r; ky++)
        for (int kx = -r; kx <= r; kx++) acc += img[(y + ky) * w + (x + kx)] * kernel[(ky + r) * ksize + (kx + r)];
      out[y * w + x] = acc;
    }
  double s = 0;
  for (size_t i = 0; i < out.size(); i += 7) s += out[i];
  c.add(s);
}

// ---- ground_segment ----------------------------------------------------------

// RANSAC plane fit with a fixed seed, so the chosen samples, and so the
// result, are the same every time.
void ground_segment(const std::vector<P3>& pts, int iters, Checksum& c) {
  Rng r(7);
  size_t best = 0;
  double bn[4] = {0, 0, 1, 0};
  for (int it = 0; it < iters; it++) {
    const P3 &a = pts[r.next() % pts.size()], &b = pts[r.next() % pts.size()], &d = pts[r.next() % pts.size()];
    double ux = b.x - a.x, uy = b.y - a.y, uz = b.z - a.z, vx = d.x - a.x, vy = d.y - a.y, vz = d.z - a.z;
    double nx = uy * vz - uz * vy, ny = uz * vx - ux * vz, nz = ux * vy - uy * vx;
    double norm = std::sqrt(nx * nx + ny * ny + nz * nz);
    if (norm < 1e-9) continue;
    nx /= norm, ny /= norm, nz /= norm;
    double off = -(nx * a.x + ny * a.y + nz * a.z);
    size_t inliers = 0;
    for (const auto& p : pts)
      if (std::fabs(nx * p.x + ny * p.y + nz * p.z + off) < 0.1) inliers++;
    if (inliers > best) best = inliers, bn[0] = nx, bn[1] = ny, bn[2] = nz, bn[3] = off;
  }
  c.add(static_cast<uint64_t>(best));
  for (double v : bn) c.add(std::fabs(v));
}

// ---- occupancy_raycast -------------------------------------------------------

void occupancy_raycast(int grid, int beams, int scans, Checksum& c) {
  std::vector<float> logodds(static_cast<size_t>(grid) * grid, 0.f);
  Rng r(11 + static_cast<uint64_t>(grid));
  int cx = grid / 2, cy = grid / 2;
  for (int s = 0; s < scans; s++) {
    for (int b = 0; b < beams; b++) {
      double ang = 2 * M_PI * b / beams;
      double range = r.range(grid * 0.1, grid * 0.45);
      int x1 = cx + static_cast<int>(range * std::cos(ang)), y1 = cy + static_cast<int>(range * std::sin(ang));
      int x = cx, y = cy, dx = std::abs(x1 - x), dy = -std::abs(y1 - y), sx = x < x1 ? 1 : -1, sy = y < y1 ? 1 : -1, err = dx + dy;
      while (x != x1 || y != y1) {
        logodds[static_cast<size_t>(y) * grid + x] -= 0.4f;
        int e2 = 2 * err;
        if (e2 >= dy) err += dy, x += sx;
        if (e2 <= dx) err += dx, y += sy;
      }
      logodds[static_cast<size_t>(y1) * grid + x1] += 0.85f;
    }
  }
  double s = 0;
  for (size_t i = 0; i < logodds.size(); i += 3) s += logodds[i];
  c.add(s);
}

// ---- astar_plan --------------------------------------------------------------

void astar_plan(int n, double density, Checksum& c) {
  Rng r(5 + static_cast<uint64_t>(n));
  std::vector<uint8_t> blocked(static_cast<size_t>(n) * n);
  for (auto& b : blocked) b = r.uniform() < density;
  blocked[0] = blocked[static_cast<size_t>(n) * n - 1] = 0;
  std::vector<int> g(blocked.size(), std::numeric_limits<int>::max());
  using Node = std::pair<int, int>;
  std::priority_queue<Node, std::vector<Node>, std::greater<Node>> open;
  auto h = [&](int i) { return (n - 1 - i % n) + (n - 1 - i / n); };
  g[0] = 0;
  open.push({h(0), 0});
  int expanded = 0, goal = n * n - 1;
  while (!open.empty()) {
    auto [f, cur] = open.top();
    open.pop();
    if (f - h(cur) > g[cur]) continue;
    expanded++;
    if (cur == goal) break;
    int x = cur % n, y = cur / n;
    const int dx[4] = {1, -1, 0, 0}, dy[4] = {0, 0, 1, -1};
    for (int k = 0; k < 4; k++) {
      int nx = x + dx[k], ny = y + dy[k];
      if (nx < 0 || ny < 0 || nx >= n || ny >= n) continue;
      int ni = ny * n + nx;
      if (blocked[ni] || g[cur] + 1 >= g[ni]) continue;
      g[ni] = g[cur] + 1;
      open.push({g[ni] + h(ni), ni});
    }
  }
  c.add(static_cast<uint64_t>(expanded));
  c.add(static_cast<uint64_t>(g[goal] == std::numeric_limits<int>::max() ? 0 : g[goal]));
}

// ---- ekf_fusion --------------------------------------------------------------

// A constant velocity filter over position and velocity in three axes, with
// dense 6x6 algebra, which is what dominates a small EKF's cost.
void ekf_fusion(int steps, Checksum& c) {
  constexpr int N = 6;
  using M = std::array<std::array<double, N>, N>;
  auto mul = [](const M& a, const M& b) {
    M o{};
    for (int i = 0; i < N; i++)
      for (int k = 0; k < N; k++)
        for (int j = 0; j < N; j++) o[i][j] += a[i][k] * b[k][j];
    return o;
  };
  auto tr = [](const M& a) {
    M o{};
    for (int i = 0; i < N; i++)
      for (int j = 0; j < N; j++) o[i][j] = a[j][i];
    return o;
  };
  M F{}, P{}, Q{}, I{};
  for (int i = 0; i < N; i++) F[i][i] = P[i][i] = I[i][i] = 1, Q[i][i] = 0.01;
  for (int i = 0; i < 3; i++) F[i][i + 3] = 0.1;
  std::array<double, N> x{};
  Rng r(3 + static_cast<uint64_t>(steps));
  for (int s = 0; s < steps; s++) {
    std::array<double, N> nx{};
    for (int i = 0; i < N; i++)
      for (int j = 0; j < N; j++) nx[i] += F[i][j] * x[j];
    x = nx;
    P = mul(mul(F, P), tr(F));
    for (int i = 0; i < N; i++) P[i][i] += Q[i][i];
    // Position measurement: K = P[:, :3] / (P[:3,:3] + R), with the 3x3
    // inverse replaced by its diagonal, which keeps the cost dense and the
    // filter stable.
    for (int a = 0; a < 3; a++) {
      double z = s * 0.1 + r.range(-0.2, 0.2);
      double S = P[a][a] + 0.05;
      double innov = z - x[a];
      std::array<double, N> K{};
      for (int i = 0; i < N; i++) K[i] = P[i][a] / S;
      for (int i = 0; i < N; i++) x[i] += K[i] * innov;
      M KH{};
      for (int i = 0; i < N; i++) KH[i][a] = K[i];
      M IKH{};
      for (int i = 0; i < N; i++)
        for (int j = 0; j < N; j++) IKH[i][j] = I[i][j] - KH[i][j];
      P = mul(IKH, P);
    }
  }
  for (double v : x) c.add(v);
  c.add(P[0][0]);
}

// ---- mpc_rollout -------------------------------------------------------------

void mpc_rollout(int samples, int horizon, Checksum& c) {
  Rng r(9 + static_cast<uint64_t>(samples));
  double best = std::numeric_limits<double>::max();
  int bestIdx = -1;
  for (int s = 0; s < samples; s++) {
    double x = 0, y = 0, yaw = 0, v = 5, cost = 0;
    for (int t = 0; t < horizon; t++) {
      double steer = r.range(-0.4, 0.4), accel = r.range(-2, 2);
      x += v * std::cos(yaw) * 0.1;
      y += v * std::sin(yaw) * 0.1;
      yaw += v / 2.7 * std::tan(steer) * 0.1;
      v = std::max(0.0, v + accel * 0.1);
      double ref = std::sin(x * 0.1) * 2;
      cost += (y - ref) * (y - ref) + 0.1 * steer * steer + 0.01 * accel * accel;
    }
    if (cost < best) best = cost, bestIdx = s;
  }
  c.add(best);
  c.add(static_cast<uint64_t>(bestIdx));
}

// ---- cluster_euclidean ---------------------------------------------------------

void cluster_euclidean(const std::vector<P3>& pts, double tol, Checksum& c) {
  std::vector<int> above;
  for (size_t i = 0; i < pts.size(); i++)
    if (pts[i].z > 0.15) above.push_back(static_cast<int>(i));
  auto cell = [&](double v) { return static_cast<int64_t>(std::floor(v / tol)); };
  auto key = [&](int64_t x, int64_t y, int64_t z) { return (static_cast<uint64_t>(x + (1 << 20)) << 42) ^ (static_cast<uint64_t>(y + (1 << 20)) << 21) ^ static_cast<uint64_t>(z + (1 << 20)); };
  std::unordered_map<uint64_t, std::vector<int>> grid;
  for (int i : above) grid[key(cell(pts[i].x), cell(pts[i].y), cell(pts[i].z))].push_back(i);
  std::vector<int> label(pts.size(), -1);
  int clusters = 0;
  size_t largest = 0;
  for (int seed : above) {
    if (label[seed] >= 0) continue;
    std::vector<int> stack = {seed};
    label[seed] = clusters;
    size_t size = 0;
    while (!stack.empty()) {
      int i = stack.back();
      stack.pop_back();
      size++;
      int64_t cx = cell(pts[i].x), cy = cell(pts[i].y), cz = cell(pts[i].z);
      for (int64_t dx = -1; dx <= 1; dx++)
        for (int64_t dy = -1; dy <= 1; dy++)
          for (int64_t dz = -1; dz <= 1; dz++) {
            auto it = grid.find(key(cx + dx, cy + dy, cz + dz));
            if (it == grid.end()) continue;
            for (int j : it->second) {
              if (label[j] >= 0) continue;
              double ddx = pts[i].x - pts[j].x, ddy = pts[i].y - pts[j].y, ddz = pts[i].z - pts[j].z;
              if (ddx * ddx + ddy * ddy + ddz * ddz <= tol * tol) label[j] = clusters, stack.push_back(j);
            }
          }
    }
    largest = std::max(largest, size);
    clusters++;
  }
  c.add(static_cast<uint64_t>(clusters));
  c.add(static_cast<uint64_t>(largest));
}

struct Profile {
  std::string family;
  std::function<std::function<void(Checksum&)>(uint64_t seed)> prepare;
};

// Repetitions per profile, calibrated once on an Apple M4 so each run of a
// profile takes about 40 ms there. They are fixed in the source rather than
// recalibrated on each rig, because a rig that recalibrated would be measuring
// a different amount of work and every comparison across rigs would be void.
const std::map<std::string, int> kReps = {
      {"astar_plan_l", 3},
      {"astar_plan_m", 35},
      {"astar_plan_s", 95},
      {"cluster_euclidean_l", 5},
      {"cluster_euclidean_m", 8},
      {"cluster_euclidean_s", 22},
      {"conv2d_l", 16},
      {"conv2d_m", 59},
      {"conv2d_s", 648},
      {"ekf_fusion_l", 172},
      {"ekf_fusion_m", 118},
      {"ekf_fusion_s", 613},
      {"ground_segment_l", 341},
      {"ground_segment_m", 318},
      {"ground_segment_s", 1359},
      {"icp_step_l", 2},
      {"icp_step_m", 5},
      {"icp_step_s", 16},
      {"knn_kdtree_l", 3},
      {"knn_kdtree_m", 5},
      {"knn_kdtree_s", 29},
      {"mpc_rollout_l", 36},
      {"mpc_rollout_m", 58},
      {"mpc_rollout_s", 95},
      {"occupancy_raycast_l", 28},
      {"occupancy_raycast_m", 83},
      {"occupancy_raycast_s", 259},
      {"voxel_downsample_l", 18},
      {"voxel_downsample_m", 23},
      {"voxel_downsample_s", 50}};

std::map<std::string, Profile> profiles() {
  std::map<std::string, Profile> m;
  const char* sizes[3] = {"s", "m", "l"};
  for (int i = 0; i < 3; i++) {
    std::string sz = sizes[i];
    int f = 1 << i;  // 1, 2, 4: each size handles twice the data of the last
    m["voxel_downsample_" + sz] = {"voxel_downsample", [=](uint64_t seed) {
                                     Rng r(seed);
                                     auto pts = cloud(r, 8000 * f);
                                     return std::function<void(Checksum&)>([pts](Checksum& c) { voxel_downsample(pts, 0.5, c); });
                                   }};
    m["knn_kdtree_" + sz] = {"knn_kdtree", [=](uint64_t seed) {
                               Rng r(seed);
                               auto pts = cloud(r, 4000 * f);
                               auto q = cloud(r, 500 * f);
                               return std::function<void(Checksum&)>([pts, q](Checksum& c) { knn_search(pts, q, 8, c); });
                             }};
    m["icp_step_" + sz] = {"icp_step", [=](uint64_t seed) {
                             Rng r(seed);
                             auto target = cloud(r, 3000 * f);
                             auto source = target;
                             for (auto& p : source) p.x += 0.3, p.y -= 0.2, p.z += 0.01;
                             return std::function<void(Checksum&)>([target, source](Checksum& c) { icp_step(target, source, c); });
                           }};
    m["conv2d_" + sz] = {"conv2d", [=](uint64_t seed) {
                           Rng r(seed);
                           int w = 160 * (1 << i), h = 120 * (1 << i);
                           std::vector<float> img(static_cast<size_t>(w) * h);
                           for (auto& v : img) v = static_cast<float>(r.uniform());
                           return std::function<void(Checksum&)>([img, w, h](Checksum& c) { conv2d(img, w, h, 5, c); });
                         }};
    m["ground_segment_" + sz] = {"ground_segment", [=](uint64_t seed) {
                                   Rng r(seed);
                                   auto pts = cloud(r, 4000 * f);
                                   return std::function<void(Checksum&)>([pts](Checksum& c) { ground_segment(pts, 20, c); });
                                 }};
    m["occupancy_raycast_" + sz] = {"occupancy_raycast", [=](uint64_t) {
                                      int grid = 200 * (1 << i);
                                      return std::function<void(Checksum&)>([grid](Checksum& c) { occupancy_raycast(grid, 360, 4, c); });
                                    }};
    m["astar_plan_" + sz] = {"astar_plan", [=](uint64_t) {
                               int n = 100 * (1 << i);
                               return std::function<void(Checksum&)>([n](Checksum& c) { astar_plan(n, 0.25, c); });
                             }};
    m["ekf_fusion_" + sz] = {"ekf_fusion", [=](uint64_t) {
                               int steps = 400 * f;
                               return std::function<void(Checksum&)>([steps](Checksum& c) { ekf_fusion(steps, c); });
                             }};
    m["mpc_rollout_" + sz] = {"mpc_rollout", [=](uint64_t) {
                                int samples = 500 * f;
                                return std::function<void(Checksum&)>([samples](Checksum& c) { mpc_rollout(samples, 40, c); });
                              }};
    m["cluster_euclidean_" + sz] = {"cluster_euclidean", [=](uint64_t seed) {
                                      Rng r(seed);
                                      auto pts = cloud(r, 6000 * f);
                                      return std::function<void(Checksum&)>([pts](Checksum& c) { cluster_euclidean(pts, 0.6, c); });
                                    }};
  }
  return m;
}

int usage() {
  std::fprintf(stderr,
               "usage: avbench --profile NAME [--seed N] [--slowdown PERCENT] [--reps N]\n"
               "       avbench --profile NAME --loop HZ [--cycles N] [--frames N]\n"
               "       avbench --list\n");
  return 2;
}

// runLoop is the hardware-in-the-loop mode, with the hardware simulated: a
// sensor at a fixed rate, standing in for the camera or lidar a real rig
// would have attached. Every cycle wakes on an absolute schedule, processes
// the next frame of a recording made before the loop starts, and must finish
// within the period. The recording is a fixed number of frames generated from
// consecutive seeds, replayed in order, so every run sees the same input.
//
// What a periodic stage is judged on is not its mean: it is how late it
// starts (jitter against the ideal schedule), how long each cycle takes, and
// how many cycles miss their deadline.
int runLoop(const Profile& p, uint64_t seed, double hz, int cycles, int frames) {
  using clock = std::chrono::steady_clock;
  std::vector<std::function<void(Checksum&)>> recording;
  recording.reserve(frames);
  for (int f = 0; f < frames; f++) recording.push_back(p.prepare(seed + static_cast<uint64_t>(f)));

  const auto period = std::chrono::nanoseconds(static_cast<int64_t>(1e9 / hz));
  std::vector<int64_t> compute(cycles), lateness(cycles);
  int misses = 0;
  Checksum sum;
  auto next = clock::now() + period;
  for (int c = 0; c < cycles; c++) {
    std::this_thread::sleep_until(next);
    auto woke = clock::now();
    Checksum frame;
    recording[c % frames](frame);
    auto done = clock::now();
    sum.add(frame.h);
    lateness[c] = std::chrono::duration_cast<std::chrono::nanoseconds>(woke - next).count();
    compute[c] = std::chrono::duration_cast<std::chrono::nanoseconds>(done - woke).count();
    if (done > next + period) misses++;
    next += period;
    // A cycle that overran does not try to catch up by running the next ones
    // back to back, which would hide the overrun in the next cycles' jitter.
    while (next < clock::now()) {
      next += period;
    }
  }
  auto pct = [](std::vector<int64_t> v, double q) {
    std::sort(v.begin(), v.end());
    return v[static_cast<size_t>(q * (v.size() - 1))];
  };
  std::printf("BENCHGRID_METRIC cycle_p50_ns %lld\n", static_cast<long long>(pct(compute, 0.5)));
  std::printf("BENCHGRID_METRIC cycle_p99_ns %lld\n", static_cast<long long>(pct(compute, 0.99)));
  std::printf("BENCHGRID_METRIC jitter_p99_ns %lld\n", static_cast<long long>(pct(lateness, 0.99)));
  std::printf("BENCHGRID_METRIC jitter_max_ns %lld\n", static_cast<long long>(pct(lateness, 1.0)));
  std::printf("BENCHGRID_METRIC deadline_misses %d\n", misses);
  std::printf("BENCHGRID_CHECKSUM %016llx\n", static_cast<unsigned long long>(sum.h));
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  std::string name;
  uint64_t seed = 42;
  double slowdown = 0;
  int repsOverride = 0;
  bool list = false;
  double hz = 0;
  int cycles = 200, frames = 16;
  for (int i = 1; i < argc; i++) {
    std::string a = argv[i];
    auto val = [&]() -> const char* { return i + 1 < argc ? argv[++i] : nullptr; };
    if (a == "--list") {
      list = true;
    } else if (a == "--profile") {
      const char* v = val();
      if (!v) return usage();
      name = v;
    } else if (a == "--seed") {
      const char* v = val();
      if (!v) return usage();
      seed = std::strtoull(v, nullptr, 10);
    } else if (a == "--slowdown") {
      const char* v = val();
      if (!v) return usage();
      slowdown = std::atof(v);
    } else if (a == "--loop") {
      const char* v = val();
      if (!v) return usage();
      hz = std::atof(v);
    } else if (a == "--cycles") {
      const char* v = val();
      if (!v) return usage();
      cycles = std::atoi(v);
    } else if (a == "--frames") {
      const char* v = val();
      if (!v) return usage();
      frames = std::atoi(v);
    } else if (a == "--reps") {
      const char* v = val();
      if (!v) return usage();
      repsOverride = std::atoi(v);
    } else {
      return usage();
    }
  }
  auto all = profiles();
  if (list) {
    for (const auto& [n, p] : all) std::printf("%s\n", n.c_str());
    return 0;
  }
  auto it = all.find(name);
  if (it == all.end() || slowdown < 0 || slowdown > 100) return usage();
  if (hz > 0) {
    if (cycles < 1 || frames < 1) return usage();
    return runLoop(it->second, seed, hz, cycles, frames);
  }

  int reps = repsOverride > 0 ? repsOverride : kReps.at(name);
  auto kernel = it->second.prepare(seed);

  Checksum sum;
  auto start = std::chrono::steady_clock::now();
  for (int r = 0; r < reps; r++) {
    Checksum c;
    kernel(c);
    if (r == 0) sum = c;
  }
  if (slowdown > 0) {
    auto base = std::chrono::steady_clock::now() - start;
    auto until = start + base + std::chrono::duration_cast<std::chrono::steady_clock::duration>(base * (slowdown / 100.0));
    volatile uint64_t x = 1;
    while (std::chrono::steady_clock::now() < until)
      for (int k = 0; k < 256; k++) x = x * 6364136223846793005ULL + 1442695040888963407ULL;
  }
  auto ns = std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - start).count();

  std::printf("BENCHGRID_METRIC work_ns %lld\n", static_cast<long long>(ns));
  std::printf("BENCHGRID_CHECKSUM %016llx\n", static_cast<unsigned long long>(sum.h));
  return 0;
}
