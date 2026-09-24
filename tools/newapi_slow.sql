select
  case
    when completion_tokens < 50 then 'a <50tok'
    when completion_tokens < 200 then 'b 50-200'
    when completion_tokens < 500 then 'c 200-500'
    when completion_tokens < 2000 then 'd 500-2k'
    else 'e >2k'
  end as out_bucket,
  count(*) as n,
  round(avg(completion_tokens/use_time),1) as avg_tps,
  sum(case when completion_tokens/use_time < 100 then 1 else 0 end) as slow_n
from logs
where created_at > unix_timestamp(now()) - 86400*2 and type=2
  and model_name='deepseek-v4.1-flash' and is_stream=1 and completion_tokens > 30
group by out_bucket order by out_bucket;
