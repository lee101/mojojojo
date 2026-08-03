def shade(iterations: Int, limit: Int) -> Int:
    if iterations >= limit:
        return 0
    return 45 + (iterations * 210) // limit
