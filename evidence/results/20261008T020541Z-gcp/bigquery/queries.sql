-- totals
SELECT COUNT(*) AS runs, COUNTIF(status = "SUCCEEDED") AS succeeded, COUNT(DISTINCT rig_id) AS rigs, COUNT(DISTINCT benchmark) AS benchmarks
FROM `medi-agent-490106.benchgrid.runs`;

-- per_rig
SELECT rig_id, hardware_class, COUNT(*) AS runs, COUNTIF(status = "SUCCEEDED") AS succeeded
FROM `medi-agent-490106.benchgrid.runs`
GROUP BY 1, 2 ORDER BY 1;

-- median_work_ms_by_class: the per profile median kernel time on each class of node
SELECT benchmark, hardware_class, ROUND(APPROX_QUANTILES(value, 100)[OFFSET(50)] / 1e6, 3) AS median_work_ms, COUNT(*) AS samples
FROM `medi-agent-490106.benchgrid.samples`
WHERE metric = "work_ns" AND NOT warmup AND status = "SUCCEEDED" AND benchmark LIKE "avbench_%_m"
GROUP BY 1, 2 ORDER BY 1, 2;
