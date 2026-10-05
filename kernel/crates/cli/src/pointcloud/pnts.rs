//! 3D Tiles Batched Point Clouds（.pnts，version 1.0）最小编码器。
//!
//! FeatureTable JSON：POINTS_LENGTH / RTC_CENTER（tile 局部原点的 ECEF 或
//! ENU 坐标，供 f32 精度补偿）/ POSITION（f32×3）/ RGB（u8×3，可选）。

/// 编码单瓦片 pnts（未对齐处理内建：JSON 与二进制均按 8 字节补齐）。
/// `rtc_center` 为该瓦片点的局部中心（POSITION 均相对它）。
pub fn encode_pnts(
    points: &[[f32; 3]],
    colors: Option<&[[u8; 3]]>,
    rtc_center: [f64; 3],
) -> Result<Vec<u8>, String> {
    let n = points.len();
    if n == 0 {
        return Err("空点集不能编码为 pnts".into());
    }
    if let Some(c) = colors {
        if c.len() != n {
            return Err(format!("颜色数 {} 与点数 {n} 不一致", c.len()));
        }
    }

    // ---- feature table binary：positions (+ colors) ----
    let positions_bytes = n * 12;
    let rgb_offset = colors.map(|_| (positions_bytes + 7) / 8 * 8); // 8 字节对齐
    let mut ft_binary: Vec<u8> = Vec::with_capacity(n * 12 + n * 3);
    for p in points {
        for v in p {
            ft_binary.extend_from_slice(&v.to_le_bytes());
        }
    }
    if let Some(off) = rgb_offset {
        while ft_binary.len() < off {
            ft_binary.push(0);
        }
        for col in colors.unwrap() {
            ft_binary.extend_from_slice(col);
        }
    }
    // 尾部补齐到 8 字节（规范建议）
    while ft_binary.len() % 8 != 0 {
        ft_binary.push(0);
    }

    // ---- feature table JSON ----
    let rgb_offset_in_json = rgb_offset;
    let mut json = format!(
        r#"{{"POINTS_LENGTH":{n},"RTC_CENTER":[{},{},{}],"POSITION":{{"byteOffset":0}}"#,
        rtc_center[0], rtc_center[1], rtc_center[2]
    );
    if let Some(off) = rgb_offset_in_json {
        json.push_str(&format!(r#","RGB":{{"byteOffset":{off}}}"#));
    }
    json.push('}');
    // JSON 长度按 8 字节对齐（空格填充）
    while (json.len() + 28) % 8 != 0 {
        json.push(' ');
    }

    // ---- header（28 字节） ----
    let total = 28 + json.len() + ft_binary.len();
    let mut out = Vec::with_capacity(total);
    out.extend_from_slice(b"pnts");
    out.extend_from_slice(&1u32.to_le_bytes()); // version
    out.extend_from_slice(&(total as u32).to_le_bytes());
    out.extend_from_slice(&(json.len() as u32).to_le_bytes());
    out.extend_from_slice(&(ft_binary.len() as u32).to_le_bytes());
    out.extend_from_slice(&0u32.to_le_bytes()); // batch table json
    out.extend_from_slice(&0u32.to_le_bytes()); // batch table binary
    debug_assert_eq!(out.len(), 28);
    out.extend_from_slice(json.as_bytes());
    out.extend_from_slice(&ft_binary);
    Ok(out)
}

/// 解析 pnts（测试/校验用独立实现）。
#[cfg(test)]
pub fn decode_pnts(data: &[u8]) -> Result<(usize, [f64; 3], Vec<[f32; 3]>, Option<Vec<[u8; 3]>>), String> {
    if data.len() < 28 || &data[0..4] != b"pnts" {
        return Err("不是合法 pnts".into());
    }
    let u32le = |o: usize| u32::from_le_bytes(data[o..o + 4].try_into().unwrap());
    let json_len = u32le(12) as usize;
    let bin_len = u32le(16) as usize;
    let json: serde_json::Value =
        serde_json::from_slice(&data[28..28 + json_len]).map_err(|e| e.to_string())?;
    let n = json["POINTS_LENGTH"].as_u64().ok_or("缺 POINTS_LENGTH")? as usize;
    let rtc: Vec<f64> = json["RTC_CENTER"]
        .as_array()
        .map(|a| a.iter().map(|v| v.as_f64().unwrap_or(0.0)).collect())
        .ok_or("缺 RTC_CENTER")?;
    let bin = &data[28 + json_len..28 + json_len + bin_len];
    let mut positions = Vec::with_capacity(n);
    for i in 0..n {
        let o = i * 12;
        positions.push([
            f32::from_le_bytes(bin[o..o + 4].try_into().unwrap()),
            f32::from_le_bytes(bin[o + 4..o + 8].try_into().unwrap()),
            f32::from_le_bytes(bin[o + 8..o + 12].try_into().unwrap()),
        ]);
    }
    let colors = if json.get("RGB").is_some() {
        let off = json["RGB"]["byteOffset"].as_u64().unwrap() as usize;
        let mut c = Vec::with_capacity(n);
        for i in 0..n {
            c.push([bin[off + i * 3], bin[off + i * 3 + 1], bin[off + i * 3 + 2]]);
        }
        Some(c)
    } else {
        None
    };
    Ok((n, [rtc[0], rtc[1], rtc[2]], positions, colors))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pnts_roundtrip_with_and_without_color() {
        let pts: Vec<[f32; 3]> = (0..100).map(|i| [i as f32, i as f32 * 0.5, -i as f32]).collect();
        let cols: Vec<[u8; 3]> = (0..100).map(|i| [i as u8, 200, 50]).collect();
        let data = encode_pnts(&pts, Some(&cols), [1.5, 2.5, 3.5]).unwrap();
        // 对齐：二进制段起点（28 + json_len）为 8 的倍数，总长亦为 8 的倍数
        assert_eq!(data.len() % 8, 0);
        let json_len = u32::from_le_bytes(data[12..16].try_into().unwrap()) as usize;
        assert_eq!((json_len + 28) % 8, 0);

        let (n, rtc, pos, col) = decode_pnts(&data).unwrap();
        assert_eq!(n, 100);
        assert_eq!(rtc, [1.5, 2.5, 3.5]);
        assert_eq!(pos.len(), 100);
        assert_eq!(pos[42], pts[42]);
        assert_eq!(col.unwrap()[99], cols[99]);

        // 无颜色
        let data2 = encode_pnts(&pts, None, [0.0; 3]).unwrap();
        let (n, _, pos, col) = decode_pnts(&data2).unwrap();
        assert_eq!(n, 100);
        assert!(col.is_none());
        assert_eq!(pos[0], pts[0]);
    }

    #[test]
    fn rejects_empty_and_mismatched_colors() {
        assert!(encode_pnts(&[], None, [0.0; 3]).is_err());
        let pts = vec![[1.0f32; 3]; 3];
        assert!(encode_pnts(&pts, Some(&[[1u8; 3]; 2]), [0.0; 3]).is_err());
    }
}
