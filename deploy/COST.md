<!-- # Cost Scenario Analysis (Task 5)

> This is the deliverable for Task 5. **If you can access the data, provide specific numbers and queries. If you cannot, state your key assumptions, the data you would use to validate them, and how your conclusions would change if they proved false.** Tie each conclusion to the data and validation method instead of listing generic cost-cutting ideas. Aim for no more than 600 words.

## 1. Establish the Baseline

Which data would you inspect first to decide whether the increase is expected? How would you separate usage growth from lower efficiency or waste? Which unit would you use, such as cost per event, request, or daily active user?

## 2. Attribution Approach (Observe → Hypothesize → Verify)

Describe one investigation chain that breaks the 30% increase down to specific sources. Explain which data you would use at each step:

- which dimensions you would start with, such as service, usage type, account, Region, or tag, and how you would attribute the untagged 40%;
- how you would determine whether increases across several services share one root cause;
- which confounding factors you would rule out at each step, such as billing-period length, RI or Savings Plan amortization, and one-time charges.

## 3. Actions and Trade-offs

Choose one constraint. Give an actionable first step, a fallback option, and what you would give up:

- you want to use committed-use discounts such as RIs or Savings Plans, but the event table is scheduled to migrate next quarter;
- reducing volume at the source depends on the app team, but that team has no capacity this quarter;
- last month you expected costs to fall, but this month they reached a new high, and you must set honest expectations with your leader.

Separate one-time point optimizations from root-cause governance or long-term controls, and explain how you would prevent the cost from returning.

## 4. Supporting Experience (Optional)

Describe one real cost optimization, including the measured before-and-after result and how you confirmed that your change caused the improvement rather than a coincidental change in business volume. -->


Baseline: normalise before attributing
The headline is +30% ($40k → $52k). That number is wrong in a way that matters.

Previous month: $40k / 31 days = $1,290/day. Current: $52k / 30 days = $1,733/day. Daily run-rate is +34.3%, not +30%. The calendar is hiding about four points, not explaining them. Anyone who opens with "some of this is billing days" has the sign backwards.

Unit cost is the working measure: $ per 1k user_event writes and $ per 1M API requests. Business volume grew 5%, so unit cost rose ≈ 1.343 / 1.05 = +27.9%. That splits the problem: ~5pp is growth, ~28pp is efficiency. Only the second part is recoverable.

Unblended also has to go. RIs/SPs bought at quarter start distort it; I re-run everything on amortized and net amortized, and exclude line_item_line_item_type IN ('Fee','RIFee','Credit','Tax','Refund') to strip one-offs.

Attribution: observe → hypothesise → verify
Observe. Four services rose together — DynamoDB, S3, EKS, ELB. Four independent regressions in one month is implausible; one upstream change with fan-out is not.

Hypothesise. The app revision increased writes per business event (write amplification). One extra user_event write propagates: DynamoDB WCU → stream/backup PUTs to S3 → more objects for EMR to scan → more consumer pods on EKS → more LCU through ELB. Usage-proportional cost from a non-proportional cause.

Verify. Group the CUR by line_item_usage_type + line_item_operation + line_item_resource_id, daily, spanning the deploy date:

DynamoDB: WriteCapacityUnit-Hrs / WriteRequestUnits vs TimedStorage-ByteHrs. Storage flat + writes up ⇒ amplification, not retention.
S3: Requests-Tier1 (PUT) count vs TimedStorage. A PUT-count jump with flat bytes means more, smaller objects — which also inflates EMR scan cost.
ELB: split LCUUsage by NewConnection / ActiveConnection / ProcessedBytes. NewConnection-led ⇒ connection churn, not payload growth.
CloudWatch: ConsumedWriteCapacityUnits ÷ business events, before vs after deploy. The decisive test — if that ratio jumped ~28% while volume rose 5%, the hypothesis holds.
The untagged 40%. Tags are not required for this. line_item_resource_id gives table and bucket identity natively; EKS split cost allocation data attributes pod cost by namespace without tags. Tags matter for team chargeback, not for finding this. I backfill ownership by joining resource IDs against AWS Config inventory.

Confounders ruled out: billing days (computed above), RI/SP amortization (amortized view), one-offs (line-item-type filter), region/price changes (unit price per usage type held constant).

Trade-off: leadership wants −30%, ~15% is recoverable
I commit to 15%, with a named breakdown, and say plainly where the rest went.

Point fixes, this month, no APP capacity needed (~15%): S3 lifecycle + Intelligent-Tiering and aborting incomplete multipart uploads; DynamoDB provisioned-with-autoscaling where the write curve is now predictable; EKS rightsizing/consolidation; ELB keep-alive tuning to cut NewConnection LCU.

What I give up: I will not buy DynamoDB reserved capacity, even though it is the fastest single win. The table migrates next quarter — a 1-year commitment against a 3-month workload is a guaranteed write-off. I accept a worse rate now to avoid locking in.

Root-cause governance, needs APP (~the other 13%): batch writes, dedupe duplicate events. Deferred to next quarter's capacity, tracked as a named debt item rather than silently dropped.

Preventing regression is the actual deliverable. Publish $/1k events as a CloudWatch metric, alert at +10% week-over-week, and review it per deploy. This month's failure was not overspending — it was a revision changing unit economics and nobody noticing for 30 days. A lifecycle policy is a point fix; a unit-cost signal tied to deploys is the fix that stops the next one.