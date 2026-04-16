#!/bin/bash
set -e

# =============================================================================
# parallel unpack 端到端对比验证脚本
#
# 通过 containerd 配置项 max_concurrent_unpacks 控制并行解压并发度
#
# 用法:
#   sudo bash test-parallel-unpack.sh
#
# 可选环境变量:
#   TEST_IMAGE       - 测试镜像 (默认 docker.io/library/python:3.12-slim)
#   PARALLEL_WORKERS - 并行解压并发数 (默认 5)
# =============================================================================

CONTAINERD_BIN="$(cd "$(dirname "$0")" && pwd)/bin/containerd"
CTR_BIN="$(cd "$(dirname "$0")" && pwd)/bin/ctr"
BASE_DIR="/run/containerd-parallel-test"
ROOT="/var/lib/containerd-parallel-test"
TEST_IMAGE="${TEST_IMAGE:-docker.io/library/python:3.12-slim}"
PARALLEL_WORKERS="${PARALLEL_WORKERS:-5}"

echo "========================================="
echo " Parallel Unpack E2E Verification"
echo "========================================="
echo "containerd: $CONTAINERD_BIN"
echo "ctr:        $CTR_BIN"
echo "image:      $TEST_IMAGE"
echo "workers:    $PARALLEL_WORKERS"
echo ""

if [ ! -f "$CONTAINERD_BIN" ] || [ ! -f "$CTR_BIN" ]; then
    echo "ERROR: 请先运行 'make binaries'"
    exit 1
fi

CONTAINERD_PID=""
cleanup() {
    echo ""
    echo "[cleanup] 停止 containerd..."
    [ -n "$CONTAINERD_PID" ] && kill "$CONTAINERD_PID" 2>/dev/null || true
    [ -n "$CONTAINERD_PID" ] && wait "$CONTAINERD_PID" 2>/dev/null || true
    rm -rf "$ROOT" "$BASE_DIR"
    echo "[cleanup] 完成"
}
trap cleanup EXIT

# ---- 生成配置文件: 顺序模式 (max_concurrent_unpacks = 1) ----
gen_config() {
    local unpacks=$1
    local config_file=$2
    cat > "$config_file" <<EOF
version = 2

[plugins]
  [plugins."io.containerd.transfer.v1.local"]
    max_concurrent_downloads = 10
    max_concurrent_unpacks = ${unpacks}
EOF
}

# ---- 启动 containerd ----
start_containerd() {
    local label=$1
    local config=$2
    local log_file=$3
    local sock="${BASE_DIR}/containerd.sock"

    rm -rf "$ROOT" "$BASE_DIR"
    mkdir -p "$BASE_DIR"

    echo "[${label}] 启动 containerd (config: max_concurrent_unpacks=$(grep max_concurrent_unpacks "$config" | awk '{print $3}'))..."
    $CONTAINERD_BIN \
        --root "$ROOT" \
        --state "$BASE_DIR" \
        --address "$sock" \
        --log-level debug \
        --config "$config" \
        2>"$log_file" &
    CONTAINERD_PID=$!

    for i in $(seq 1 30); do
        [ -S "$sock" ] && break
        sleep 0.5
    done
    if [ ! -S "$sock" ]; then
        echo "ERROR: containerd 启动超时"
        cat "$log_file" | tail -20
        exit 1
    fi
    echo "[${label}] containerd 已启动 (PID=$CONTAINERD_PID)"
}

stop_containerd() {
    [ -n "$CONTAINERD_PID" ] && kill "$CONTAINERD_PID" 2>/dev/null || true
    [ -n "$CONTAINERD_PID" ] && wait "$CONTAINERD_PID" 2>/dev/null || true
    CONTAINERD_PID=""
}

SOCK="${BASE_DIR}/containerd.sock"

# =============================================================================
# 测试 1: 顺序解压 (max_concurrent_unpacks = 1)
# =============================================================================
echo "========================================="
echo " Test 1: 顺序解压 (max_concurrent_unpacks=1)"
echo "========================================="

SEQ_CONFIG="/tmp/containerd-seq.toml"
SEQ_LOG="/tmp/containerd-seq.log"
gen_config 1 "$SEQ_CONFIG"
start_containerd "seq" "$SEQ_CONFIG" "$SEQ_LOG"

echo "[seq] 拉取镜像 (transfer service 路径)..."
START_SEQ=$(date +%s%N)
$CTR_BIN -a "$SOCK" images pull --local=false "$TEST_IMAGE" 2>&1
END_SEQ=$(date +%s%N)
SEQ_MS=$(( (END_SEQ - START_SEQ) / 1000000 ))

echo "[seq] 耗时: ${SEQ_MS}ms"
echo "[seq] Snapshot chain:"
$CTR_BIN -a "$SOCK" snapshots --snapshotter overlayfs ls 2>/dev/null | head -20

# 检查日志确认走了顺序路径
SEQ_PARALLEL=$(grep -c "committed (parallel)" "$SEQ_LOG" 2>/dev/null || echo "0")
SEQ_SEQUENTIAL=$(grep -c "layer unpacked" "$SEQ_LOG" 2>/dev/null || echo "0")
echo "[seq] 并行commit日志: $SEQ_PARALLEL 条, 顺序unpack日志: $SEQ_SEQUENTIAL 条"

stop_containerd
echo ""

# =============================================================================
# 测试 2: 并行解压 (max_concurrent_unpacks = N)
# =============================================================================
echo "========================================="
echo " Test 2: 并行解压 (max_concurrent_unpacks=$PARALLEL_WORKERS)"
echo "========================================="

PAR_CONFIG="/tmp/containerd-par.toml"
PAR_LOG="/tmp/containerd-par.log"
gen_config "$PARALLEL_WORKERS" "$PAR_CONFIG"
start_containerd "par" "$PAR_CONFIG" "$PAR_LOG"

echo "[par] 拉取镜像 (transfer service 路径)..."
START_PAR=$(date +%s%N)
$CTR_BIN -a "$SOCK" images pull --local=false "$TEST_IMAGE" 2>&1
END_PAR=$(date +%s%N)
PAR_MS=$(( (END_PAR - START_PAR) / 1000000 ))

echo "[par] 耗时: ${PAR_MS}ms"
echo "[par] Snapshot chain:"
$CTR_BIN -a "$SOCK" snapshots --snapshotter overlayfs ls 2>/dev/null | head -20

# 检查日志确认走了并行路径
PAR_PARALLEL=$(grep -c "committed (parallel)" "$PAR_LOG" 2>/dev/null || echo "0")
PAR_SEQUENTIAL=$(grep -c "layer unpacked" "$PAR_LOG" 2>/dev/null || echo "0")
echo "[par] 并行commit日志: $PAR_PARALLEL 条, 顺序unpack日志: $PAR_SEQUENTIAL 条"

stop_containerd
echo ""

# =============================================================================
# 结果汇总
# =============================================================================
echo "========================================="
echo " 结果汇总"
echo "========================================="
echo ""
echo "  配置对比:"
echo "    顺序: max_concurrent_unpacks = 1"
echo "    并行: max_concurrent_unpacks = $PARALLEL_WORKERS"
echo ""
echo "  耗时对比:"
echo "    顺序解压: ${SEQ_MS}ms"
echo "    并行解压: ${PAR_MS}ms"
if [ "$PAR_MS" -gt 0 ] && command -v bc &>/dev/null; then
    SPEEDUP=$(echo "scale=2; $SEQ_MS / $PAR_MS" | bc 2>/dev/null || echo "N/A")
    echo "    加速比:   ${SPEEDUP}x"
fi
echo ""
echo "  路径验证:"
if [ "$SEQ_PARALLEL" -eq 0 ] && [ "$SEQ_SEQUENTIAL" -gt 0 ]; then
    echo "    [OK] 顺序模式: 走了串行解压路径 ($SEQ_SEQUENTIAL 层)"
else
    echo "    [WARN] 顺序模式: 未检测到预期的串行日志"
fi
if [ "$PAR_PARALLEL" -gt 0 ]; then
    echo "    [OK] 并行模式: 走了并行解压路径 ($PAR_PARALLEL 层并行commit)"
else
    echo "    [FAIL] 并行模式: 未检测到并行commit日志"
    echo "           检查日志: grep 'parallel\|rebase\|capabilities' $PAR_LOG"
fi
echo ""
echo "  详细日志:"
echo "    顺序: $SEQ_LOG"
echo "    并行: $PAR_LOG"
echo ""
echo "  调试命令:"
echo "    grep 'parallel\|rebase' $PAR_LOG"
echo "    grep 'capabilities' $PAR_LOG"
echo "========================================="
